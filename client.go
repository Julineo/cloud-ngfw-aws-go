package cloudngfw

import (
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/aws/signer/v4"
	"github.com/aws/aws-sdk-go/service/sts"

	"github.com/paloaltonetworks/cloud-ngfw-aws-go/api"
	"github.com/paloaltonetworks/cloud-ngfw-aws-go/permissions"
)

// Client is the client.
type Client struct {
	Host      string            `json:"host"`
	AccessKey string            `json:"access-key"`
	SecretKey string            `json:"secret-key"`
	Region    string            `json:"region"`
	Protocol  string            `json:"protocol"`
	Timeout   int               `json:"timeout"`
	Headers   map[string]string `json:"headers"`

	LfaArn string `json:"lfa-arn"`
	LraArn string `json:"lra-arn"`
	Arn    string `json:"arn"`

	CheckEnvironment bool `json:"-"`

	SkipVerifyCertificate bool            `json:"skip-verify-certificate"`
	Transport             *http.Transport `json:"-"`

	Logging               uint32   `json:"-"`
	LoggingFromInitialize []string `json:"logging"`

	// Configured by Initialize().
	FirewallJwt  string `json:"-"`
	RulestackJwt string `json:"-"`

	// Internal variables.
	credsFile string
	apiPrefix string
	con       *http.Client

	// Variables for testing.
	testData        [][]byte
	testErrors      []error
	testIndex       int
	authFileContent []byte
}

// Initialize opens a connection to the cloud NGFW API and retrieves all JWTs.
func (c *Client) Initialize() error {
	var err error

	if len(c.testData) > 0 {
		c.Host = "test.nz"
	} else if err = c.initCon(); err != nil {
		return err
	} else if err = c.RefreshJwts(); err != nil {
		return err
	}

	return nil
}

// Log logs an API action.
func (c *Client) Log(method, msg string, i ...interface{}) {
	switch method {
	case http.MethodGet:
		if c.Logging&LogGet != LogGet {
			return
		}
	case http.MethodPost:
		if c.Logging&LogPost != LogPost {
			return
		}
	case http.MethodPut:
		if c.Logging&LogPut != LogPut {
			return
		}
	case http.MethodDelete:
		if c.Logging&LogDelete != LogDelete {
			return
		}
	default:
		return
	}

	log.Printf("(%s) %s", method, fmt.Sprintf(msg, i...))
}

// RefreshJwts refreshes all JWTs and stores them for future API calls.
func (c *Client) RefreshJwts() error {
	if c.Logging&LogLogin == LogLogin {
		log.Printf("(login) refreshing JWTs...")
	}

	jwtReq := getJwt{
		Expires: 90,
		KeyInfo: &jwtKeyInfo{
			Region: c.Region,
			Tenant: "XY",
		},
	}

	var creds *credentials.Credentials
	if c.AccessKey != "" || c.SecretKey != "" {
		creds = credentials.NewStaticCredentials(c.AccessKey, c.SecretKey, "")
	}

	sess, err := session.NewSessionWithOptions(session.Options{
		Config: aws.Config{
			Credentials: creds,
			Region:      aws.String(c.Region),
		},
	})

	if err != nil {
		return err
	}

	svc := sts.New(sess)

	// Get a JWT that works for both firewall and rulestack admins.
	if c.Arn != "" {
		return fmt.Errorf("No endpoint yet known for shared ARN JWT retrieval")
	}

	// Get a firewall JWT.
	if c.LfaArn != "" {
		if c.Logging&LogLogin == LogLogin {
			log.Printf("(login) refreshing firewall JWT...")
		}
		result, err := svc.AssumeRole(&sts.AssumeRoleInput{
			RoleArn:         aws.String(c.LfaArn),
			RoleSessionName: aws.String("sdk_session"),
		})
		if err != nil {
			return err
		}

		var ans authResponse
		_, err = c.Communicate(
			"", http.MethodGet, []string{"v1", "mgmt", "tokens", "cloudfirewalladmin"}, jwtReq, &ans, result.Credentials)
		if err != nil {
			return err
		}

		c.FirewallJwt = ans.Resp.Jwt
	}

	// Get rulestack JWT.
	if c.LraArn != "" {
		if c.Logging&LogLogin == LogLogin {
			log.Printf("(login) refreshing rulestack JWT...")
		}
		result, err := svc.AssumeRole(&sts.AssumeRoleInput{
			RoleArn:         aws.String(c.LraArn),
			RoleSessionName: aws.String("sdk_session"),
		})
		if err != nil {
			return err
		}

		var ans authResponse
		_, err = c.Communicate(
			"", http.MethodGet, []string{"v1", "mgmt", "tokens", "cloudrulestackadmin"}, jwtReq, &ans, result.Credentials)
		if err != nil {
			return err
		}

		c.RulestackJwt = ans.Resp.Jwt
	}

	return nil
}

/*
Communicate sends information to the API.

Param auth should be one of the permissions constants.

Param method should be one of http.Method constants.

Param path should be a slice of path parts that will be joined together with the
base apiPrefix to create the final API endpoint.

Param input is an interface that can be passed in to json.Marshal() to send to the API.

Param output is a pointer to a struct that will be filled with json.Unmarshal().

Param creds is only used internally for refreshing the JWTs and can otherwise be ignored.

This function returns the content of the body from the API call and any errors that
may have been present.
*/
func (c *Client) Communicate(auth, method string, path []string, input interface{}, output api.Oker, creds ...*sts.Credentials) ([]byte, error) {
	// Sanity check the input.
	if len(creds) > 1 {
		return nil, fmt.Errorf("Only one credentials is allowed")
	}

	var err error
	var body []byte
	var data []byte

	// Convert input into JSON.
	if input != nil {
		data, err = json.Marshal(input)
		if err != nil {
			return nil, err
		}
	}
	if c.Logging&LogSend == LogSend {
		log.Printf("sending: %s", data)
	}

	// Testing.
	if len(c.testData) > 0 {
		body = []byte(`{"test"}`)
	} else {
		// Create the request.
		req, err := http.NewRequest(
			method,
			fmt.Sprintf("%s/%s", c.apiPrefix, strings.Join(path, "/")),
			strings.NewReader(string(data)),
		)
		if err != nil {
			return nil, err
		}

		// Configure headers.
		req.Header.Set("Content-Type", "application/json")
		switch auth {
		case "":
		case permissions.Firewall:
			req.Header.Set("Authorization", c.FirewallJwt)
		case permissions.Rulestack:
			req.Header.Set("Authorization", c.RulestackJwt)
		default:
			return nil, fmt.Errorf("Unknown auth type: %q", auth)
		}
		for k, v := range c.Headers {
			req.Header.Set(k, v)
		}

		// Optional: v4 sign the request.
		if len(creds) == 1 {
			prov := provider{
				Value: credentials.Value{
					AccessKeyID:     *creds[0].AccessKeyId,
					SecretAccessKey: *creds[0].SecretAccessKey,
					SessionToken:    *creds[0].SessionToken,
				},
			}
			signer := v4.NewSigner(credentials.NewCredentials(prov))
			_, err = signer.Sign(req, strings.NewReader(string(data)), "execute-api", c.Region, time.Now())
			if err != nil {
				return nil, err
			}
		}

		// Perform the API action.
		resp, err := c.con.Do(req)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		body, err = ioutil.ReadAll(resp.Body)
		if err != nil {
			return nil, err
		}
	}

	if c.Logging&LogReceive == LogReceive {
		log.Printf("received: %s", body)
	}

	if output == nil {
		return body, nil
	}

	err = json.Unmarshal(body, output)
	if err != nil {
		return body, err
	}

	if !output.Ok() {
		return body, output
	}

	return body, nil
}

/* Internal functions. */

func (c *Client) initCon() error {
	var err error
	var tout time.Duration

	// Load up the JSON config file.
	json_client := &Client{}
	if c.credsFile != "" {
		var b []byte
		if len(c.testData) == 0 {
			b, err = ioutil.ReadFile(c.credsFile)
		} else {
			b, err = c.authFileContent, nil
		}

		if err != nil {
			return err
		}

		if err = json.Unmarshal(b, &json_client); err != nil {
			return err
		}
	}

	// Host.
	if c.Host == "" {
		if val := os.Getenv("CLOUD_NGFW_HOST"); c.CheckEnvironment && val != "" {
			c.Host = val
		} else {
			c.Host = json_client.Host
		}
	}

	// Region.
	if c.Region == "" {
		if val := os.Getenv("CLOUD_NGFW_REGION"); c.CheckEnvironment && val != "" {
			c.Region = val
		} else {
			c.Region = json_client.Region
		}
	}

	// Protocol.
	if c.Protocol == "" {
		if val := os.Getenv("CLOUD_NGFW_PROTOCOL"); c.CheckEnvironment && val != "" {
			c.Protocol = val
		} else if json_client.Protocol != "" {
			c.Protocol = json_client.Protocol
		} else {
			c.Protocol = "https"
		}
	}
	if c.Protocol != "http" && c.Protocol != "https" {
		return fmt.Errorf("Invalid protocol %q; expected 'https' or 'http'", c.Protocol)
	}

	// Timeout.
	if c.Timeout == 0 {
		if val := os.Getenv("CLOUD_NGFW_TIMEOUT"); c.CheckEnvironment && val != "" {
			if ival, err := strconv.Atoi(val); err != nil {
				return fmt.Errorf("Failed to parse timeout env var as int: %s", err)
			} else {
				c.Timeout = ival
			}
		} else if json_client.Timeout != 0 {
			c.Timeout = json_client.Timeout
		} else {
			c.Timeout = 20
		}
	}
	if c.Timeout <= 0 {
		return fmt.Errorf("Timeout for %q must be a positive int", c.Host)
	}
	tout = time.Duration(time.Duration(c.Timeout) * time.Second)

	// Headers.
	if len(c.Headers) == 0 {
		if val := os.Getenv("CLOUD_NGFW_HEADERS"); c.CheckEnvironment && val != "" {
			if err := json.Unmarshal([]byte(val), &c.Headers); err != nil {
				return err
			}
		}
		if len(c.Headers) == 0 && len(json_client.Headers) > 0 {
			c.Headers = make(map[string]string)
			for k, v := range json_client.Headers {
				c.Headers[k] = v
			}
		}
	}

	// Verify cert.
	if !c.SkipVerifyCertificate {
		if val := os.Getenv("CLOUD_NGFW_VERIFY_CERTIFICATE"); c.CheckEnvironment && val != "" {
			if vcb, err := strconv.ParseBool(val); err != nil {
				return err
			} else if vcb {
				c.SkipVerifyCertificate = vcb
			}
		}
		if !c.SkipVerifyCertificate && json_client.SkipVerifyCertificate {
			c.SkipVerifyCertificate = json_client.SkipVerifyCertificate
		}
	}

	// Logging.
	if c.Logging == 0 {
		var ll []string
		if val := os.Getenv("CLOUD_NGFW_LOGGING"); c.CheckEnvironment && val != "" {
			ll = strings.Split(val, ",")
		} else {
			ll = json_client.LoggingFromInitialize
		}
		if len(ll) > 0 {
			var lv uint32
			for _, x := range ll {
				switch x {
				case "quiet":
					lv |= LogQuiet
				case "login":
					lv |= LogLogin
				case "get":
					lv |= LogGet
				case "post":
					lv |= LogPost
				case "put":
					lv |= LogPut
				case "delete":
					lv |= LogDelete
				case "path":
					lv |= LogPath
				case "send":
					lv |= LogSend
				case "receive":
					lv |= LogReceive
				default:
					return fmt.Errorf("Unknown logging requested: %s", x)
				}
			}
			c.Logging = lv
		} else {
			c.Logging = LogLogin | LogPost | LogPut | LogDelete
		}
	}

	// Setup the https client.
	if c.Transport == nil {
		c.Transport = &http.Transport{
			Proxy: http.ProxyFromEnvironment,
			TLSClientConfig: &tls.Config{
				InsecureSkipVerify: c.SkipVerifyCertificate,
			},
		}
	}
	c.con = &http.Client{
		Transport: c.Transport,
		Timeout:   tout,
	}

	// Sanity checks.
	if c.Region == "" {
		return fmt.Errorf("No region specified")
	} else if c.Host == "" {
		return fmt.Errorf("No host specified")
	}

	// Configure the uri prefix.
	c.apiPrefix = fmt.Sprintf("%s://%s", c.Protocol, c.Host)
	//c.apiPrefix = fmt.Sprintf("%s://api.%s.aws.cloudngfw.com", c.Protocol, c.Region)

	return nil
}
