package influxdb

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/vault/sdk/database/newdbplugin"

	dbtesting "github.com/hashicorp/vault/sdk/database/newdbplugin/testing"

	"github.com/hashicorp/errwrap"
	"github.com/hashicorp/vault/helper/testhelpers/docker"
	influx "github.com/influxdata/influxdb/client/v2"
)

const createUserStatements = `CREATE USER "{{username}}" WITH PASSWORD '{{password}}';GRANT ALL ON "vault" TO "{{username}}";`

type Config struct {
	docker.ServiceURL
	Username string
	Password string
}

var _ docker.ServiceConfig = &Config{}

func (c *Config) apiConfig() influx.HTTPConfig {
	return influx.HTTPConfig{
		Addr:     c.URL().String(),
		Username: c.Username,
		Password: c.Password,
	}
}

func (c *Config) connectionParams() map[string]interface{} {
	pieces := strings.Split(c.Address(), ":")
	port, _ := strconv.Atoi(pieces[1])
	return map[string]interface{}{
		"host":     pieces[0],
		"port":     port,
		"username": c.Username,
		"password": c.Password,
	}
}

func prepareInfluxdbTestContainer(t *testing.T) (func(), *Config) {
	c := &Config{
		Username: "influx-root",
		Password: "influx-root",
	}
	if host := os.Getenv("INFLUXDB_HOST"); host != "" {
		c.ServiceURL = *docker.NewServiceURL(url.URL{Scheme: "http", Host: host})
		return func() {}, c
	}

	runner, err := docker.NewServiceRunner(docker.RunOptions{
		ImageRepo: "influxdb",
		ImageTag:  "alpine",
		Env: []string{
			"INFLUXDB_DB=vault",
			"INFLUXDB_ADMIN_USER=" + c.Username,
			"INFLUXDB_ADMIN_PASSWORD=" + c.Password,
			"INFLUXDB_HTTP_AUTH_ENABLED=true"},
		Ports: []string{"8086/tcp"},
	})
	if err != nil {
		t.Fatalf("Could not start docker InfluxDB: %s", err)
	}
	svc, err := runner.StartService(context.Background(), func(ctx context.Context, host string, port int) (docker.ServiceConfig, error) {
		c.ServiceURL = *docker.NewServiceURL(url.URL{
			Scheme: "http",
			Host:   fmt.Sprintf("%s:%d", host, port),
		})
		cli, err := influx.NewHTTPClient(c.apiConfig())
		if err != nil {
			return nil, errwrap.Wrapf("error creating InfluxDB client: {{err}}", err)
		}
		defer cli.Close()
		_, _, err = cli.Ping(1)
		if err != nil {
			return nil, errwrap.Wrapf("error checking cluster status: {{err}}", err)
		}

		return c, nil
	})
	if err != nil {
		t.Fatalf("Could not start docker InfluxDB: %s", err)
	}

	return svc.Cleanup, svc.Config.(*Config)
}

func TestInfluxdb_Initialize(t *testing.T) {
	cleanup, config := prepareInfluxdbTestContainer(t)
	defer cleanup()

	type testCase struct {
		req               newdbplugin.InitializeRequest
		expectedResponse  newdbplugin.InitializeResponse
		expectErr         bool
		expectInitialized bool
	}

	tests := map[string]testCase{
		"port is an int": {
			req: newdbplugin.InitializeRequest{
				Config:           makeConfig(config.connectionParams()),
				VerifyConnection: true,
			},
			expectedResponse: newdbplugin.InitializeResponse{
				Config: config.connectionParams(),
			},
			expectErr:         false,
			expectInitialized: true,
		},
		"port is a string": {
			req: newdbplugin.InitializeRequest{
				Config:           makeConfig(config.connectionParams(), "port", strconv.Itoa(config.connectionParams()["port"].(int))),
				VerifyConnection: true,
			},
			expectedResponse: newdbplugin.InitializeResponse{
				Config: config.connectionParams(),
			},
			expectErr:         false,
			expectInitialized: true,
		},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			db := new()
			defer dbtesting.AssertClose(t, db)

			req := newdbplugin.InitializeRequest{
				Config:           config.connectionParams(),
				VerifyConnection: true,
			}
			resp, err := db.Initialize(context.Background(), req)
			if test.expectErr && err == nil {
				t.Fatalf("err expected, got nil")
			}
			if !test.expectErr && err != nil {
				t.Fatalf("no error expected, got: %s", err)
			}

			if !reflect.DeepEqual(resp, test.expectedResponse) {
				t.Fatalf("Actual response: %#v\nExpected response: %#v", resp, test.expectedResponse)
			}

			if test.expectInitialized && !db.Initialized {
				t.Fatalf("Database should be initialized but wasn't")
			} else if !test.expectInitialized && db.Initialized {
				t.Fatalf("Database was initiailized when it shouldn't")
			}
		})
	}

	db := new()
	req := newdbplugin.InitializeRequest{
		Config:           config.connectionParams(),
		VerifyConnection: true,
	}
	_, err := db.Initialize(context.Background(), req)
	if err != nil {
		t.Fatalf("err: %s", err)
	}

	if !db.Initialized {
		t.Fatal("Database should be initialized")
	}

	err = db.Close()
	if err != nil {
		t.Fatalf("err: %s", err)
	}

	connectionParams := config.connectionParams()
	connectionParams["port"] = strconv.Itoa(connectionParams["port"].(int))
	req = newdbplugin.InitializeRequest{
		Config:           connectionParams,
		VerifyConnection: true,
	}
	_, err = db.Initialize(context.Background(), req)
	if err != nil {
		t.Fatalf("err: %s", err)
	}
}

func makeConfig(rootConfig map[string]interface{}, keyValues ...interface{}) map[string]interface{} {
	if len(keyValues)%2 != 0 {
		panic("makeConfig must be provided with key and value pairs")
	}

	// Make a copy of the map so there isn't a chance of test bleedover between maps
	config := make(map[string]interface{}, len(rootConfig)+(len(keyValues)/2))
	for k, v := range rootConfig {
		config[k] = v
	}
	for i := 0; i < len(keyValues); i += 2 {
		k := keyValues[i].(string) // Will panic if the key field isn't a string and that's fine in a test
		v := keyValues[i+1]
		config[k] = v
	}
	return config
}

func TestInfluxdb_CreateUser(t *testing.T) {
	cleanup, config := prepareInfluxdbTestContainer(t)
	defer cleanup()

	db := new()
	req := newdbplugin.InitializeRequest{
		Config:           config.connectionParams(),
		VerifyConnection: true,
	}
	dbtesting.AssertInitialize(t, db, req)

	password := "nuozxby98523u89bdfnkjl"
	newUserReq := newdbplugin.NewUserRequest{
		UsernameConfig: newdbplugin.UsernameMetadata{
			DisplayName: "test",
			RoleName:    "test",
		},
		Statements: newdbplugin.Statements{
			Commands: []string{createUserStatements},
		},
		Password:   password,
		Expiration: time.Now().Add(1 * time.Minute),
	}
	resp := dbtesting.AssertNewUser(t, db, newUserReq)

	if resp.Username == "" {
		t.Fatalf("Missing username")
	}

	assertCredsExist(t, config.URL().String(), resp.Username, password)
}

func TestUpdateUser_expiration(t *testing.T) {
	// This test should end up with a no-op since the expiration doesn't do anything in Influx

	cleanup, config := prepareInfluxdbTestContainer(t)
	defer cleanup()

	db := new()
	req := newdbplugin.InitializeRequest{
		Config:           config.connectionParams(),
		VerifyConnection: true,
	}
	dbtesting.AssertInitialize(t, db, req)

	password := "nuozxby98523u89bdfnkjl"
	newUserReq := newdbplugin.NewUserRequest{
		UsernameConfig: newdbplugin.UsernameMetadata{
			DisplayName: "test",
			RoleName:    "test",
		},
		Statements: newdbplugin.Statements{
			Commands: []string{createUserStatements},
		},
		Password:   password,
		Expiration: time.Now().Add(1 * time.Minute),
	}
	newUserResp := dbtesting.AssertNewUser(t, db, newUserReq)

	assertCredsExist(t, config.URL().String(), newUserResp.Username, password)

	renewReq := newdbplugin.UpdateUserRequest{
		Username: newUserResp.Username,
		Expiration: &newdbplugin.ChangeExpiration{
			NewExpiration: time.Now().Add(5 * time.Minute),
		},
	}
	dbtesting.AssertUpdateUser(t, db, renewReq)

	// Make sure the user hasn't changed
	assertCredsExist(t, config.URL().String(), newUserResp.Username, password)
}

func TestUpdateUser_password(t *testing.T) {
	cleanup, config := prepareInfluxdbTestContainer(t)
	defer cleanup()

	db := new()
	req := newdbplugin.InitializeRequest{
		Config:           config.connectionParams(),
		VerifyConnection: true,
	}
	dbtesting.AssertInitialize(t, db, req)

	initialPassword := "nuozxby98523u89bdfnkjl"
	newUserReq := newdbplugin.NewUserRequest{
		UsernameConfig: newdbplugin.UsernameMetadata{
			DisplayName: "test",
			RoleName:    "test",
		},
		Statements: newdbplugin.Statements{
			Commands: []string{createUserStatements},
		},
		Password:   initialPassword,
		Expiration: time.Now().Add(1 * time.Minute),
	}
	newUserResp := dbtesting.AssertNewUser(t, db, newUserReq)

	assertCredsExist(t, config.URL().String(), newUserResp.Username, initialPassword)

	newPassword := "y89qgmbzadiygry8uazodijnb"
	newPasswordReq := newdbplugin.UpdateUserRequest{
		Username: newUserResp.Username,
		Password: &newdbplugin.ChangePassword{
			NewPassword: newPassword,
		},
	}
	dbtesting.AssertUpdateUser(t, db, newPasswordReq)

	assertCredsDoNotExist(t, config.URL().String(), newUserResp.Username, initialPassword)
	assertCredsExist(t, config.URL().String(), newUserResp.Username, newPassword)
}

// TestInfluxdb_RevokeDeletedUser tests attempting to revoke a user that was
// deleted externally. Guards against a panic, see
// https://github.com/hashicorp/vault/issues/6734
func TestInfluxdb_RevokeDeletedUser(t *testing.T) {
	cleanup, config := prepareInfluxdbTestContainer(t)
	defer cleanup()

	db := new()
	req := newdbplugin.InitializeRequest{
		Config:           config.connectionParams(),
		VerifyConnection: true,
	}
	dbtesting.AssertInitialize(t, db, req)

	initialPassword := "nuozxby98523u89bdfnkjl"
	newUserReq := newdbplugin.NewUserRequest{
		UsernameConfig: newdbplugin.UsernameMetadata{
			DisplayName: "test",
			RoleName:    "test",
		},
		Statements: newdbplugin.Statements{
			Commands: []string{createUserStatements},
		},
		Password:   initialPassword,
		Expiration: time.Now().Add(1 * time.Minute),
	}
	newUserResp := dbtesting.AssertNewUser(t, db, newUserReq)

	assertCredsExist(t, config.URL().String(), newUserResp.Username, initialPassword)

	// call cleanup to remove database
	cleanup()

	assertCredsDoNotExist(t, config.URL().String(), newUserResp.Username, initialPassword)

	// attempt to revoke the user after database is gone
	delReq := newdbplugin.DeleteUserRequest{
		Username: newUserResp.Username,
	}
	_, err := db.DeleteUser(context.Background(), delReq)
	if err == nil {
		t.Fatalf("Expected err, got nil")
	}
	assertCredsDoNotExist(t, config.URL().String(), newUserResp.Username, initialPassword)
}

func TestInfluxdb_RevokeUser(t *testing.T) {
	cleanup, config := prepareInfluxdbTestContainer(t)
	defer cleanup()

	db := new()
	req := newdbplugin.InitializeRequest{
		Config:           config.connectionParams(),
		VerifyConnection: true,
	}
	dbtesting.AssertInitialize(t, db, req)

	initialPassword := "nuozxby98523u89bdfnkjl"
	newUserReq := newdbplugin.NewUserRequest{
		UsernameConfig: newdbplugin.UsernameMetadata{
			DisplayName: "test",
			RoleName:    "test",
		},
		Statements: newdbplugin.Statements{
			Commands: []string{createUserStatements},
		},
		Password:   initialPassword,
		Expiration: time.Now().Add(1 * time.Minute),
	}
	newUserResp := dbtesting.AssertNewUser(t, db, newUserReq)

	assertCredsExist(t, config.URL().String(), newUserResp.Username, initialPassword)

	// attempt to revoke the user after database is gone
	delReq := newdbplugin.DeleteUserRequest{
		Username: newUserResp.Username,
	}
	_, err := db.DeleteUser(context.Background(), delReq)
	if err != nil {
		t.Fatalf("Error deleting user: %s", err)
	}
	assertCredsDoNotExist(t, config.URL().String(), newUserResp.Username, initialPassword)
}
func assertCredsExist(t testing.TB, address, username, password string) {
	t.Helper()
	err := testCredsExist(address, username, password)
	if err != nil {
		t.Fatalf("Could not log in as %q", username)
	}
}

func assertCredsDoNotExist(t testing.TB, address, username, password string) {
	t.Helper()
	err := testCredsExist(address, username, password)
	if err == nil {
		t.Fatalf("Able to log in as %q when it shouldn't", username)
	}
}

func testCredsExist(address, username, password string) error {
	conf := influx.HTTPConfig{
		Addr:     address,
		Username: username,
		Password: password,
	}
	cli, err := influx.NewHTTPClient(conf)
	if err != nil {
		return errwrap.Wrapf("Error creating InfluxDB Client: ", err)
	}
	defer cli.Close()
	_, _, err = cli.Ping(1)
	if err != nil {
		return errwrap.Wrapf("error checking server ping: {{err}}", err)
	}
	q := influx.NewQuery("SHOW SERIES ON vault", "", "")
	response, err := cli.Query(q)
	if err != nil {
		return errwrap.Wrapf("error querying influxdb server: {{err}}", err)
	}
	if response != nil && response.Error() != nil {
		return errwrap.Wrapf("error using the correct influx database: {{err}}", response.Error())
	}
	return nil
}
