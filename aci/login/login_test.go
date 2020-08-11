/*
   Copyright 2020 Docker, Inc.

   Licensed under the Apache License, Version 2.0 (the "License");
   you may not use this file except in compliance with the License.
   You may obtain a copy of the License at

       http://www.apache.org/licenses/LICENSE-2.0

   Unless required by applicable law or agreed to in writing, software
   distributed under the License is distributed on an "AS IS" BASIS,
   WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
   See the License for the specific language governing permissions and
   limitations under the License.
*/

package login

import (
	"context"
	"io/ioutil"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/stretchr/testify/mock"
	"gotest.tools/v3/assert"

	"golang.org/x/oauth2"
)

func testLoginService(t *testing.T, m *MockAzureHelper) (*AzureLoginService, error) {
	dir, err := ioutil.TempDir("", "test_store")
	if err != nil {
		return nil, err
	}
	t.Cleanup(func() {
		_ = os.RemoveAll(dir)
	})
	return newAzureLoginServiceFromPath(filepath.Join(dir, tokenStoreFilename), m)
}

func TestRefreshInValidToken(t *testing.T) {
	data := refreshTokenData("refreshToken")
	m := &MockAzureHelper{}
	m.On("queryToken", data, "123456").Return(azureToken{
		RefreshToken: "newRefreshToken",
		AccessToken:  "newAccessToken",
		ExpiresIn:    3600,
		Foci:         "1",
	}, nil)

	azureLogin, err := testLoginService(t, m)
	assert.NilError(t, err)
	err = azureLogin.tokenStore.writeLoginInfo(TokenInfo{
		TenantID: "123456",
		Token: oauth2.Token{
			AccessToken:  "accessToken",
			RefreshToken: "refreshToken",
			Expiry:       time.Now().Add(-1 * time.Hour),
			TokenType:    "Bearer",
		},
	})
	assert.NilError(t, err)

	token, _ := azureLogin.GetValidToken()

	assert.Equal(t, token.AccessToken, "newAccessToken")
	assert.Assert(t, time.Now().Add(3500*time.Second).Before(token.Expiry))

	storedToken, _ := azureLogin.tokenStore.readToken()
	assert.Equal(t, storedToken.Token.AccessToken, "newAccessToken")
	assert.Equal(t, storedToken.Token.RefreshToken, "newRefreshToken")
	assert.Assert(t, time.Now().Add(3500*time.Second).Before(storedToken.Token.Expiry))
}

func TestClearErrorMessageIfNotAlreadyLoggedIn(t *testing.T) {
	dir, err := ioutil.TempDir("", "test_store")
	assert.NilError(t, err)
	t.Cleanup(func() {
		_ = os.RemoveAll(dir)
	})
	_, err = newAuthorizerFromLoginStorePath(filepath.Join(dir, tokenStoreFilename))
	assert.ErrorContains(t, err, "not logged in to azure, you need to run \"docker login azure\" first")
}

func TestDoesNotRefreshValidToken(t *testing.T) {
	expiryDate := time.Now().Add(1 * time.Hour)
	azureLogin, err := testLoginService(t, nil)
	assert.NilError(t, err)
	err = azureLogin.tokenStore.writeLoginInfo(TokenInfo{
		TenantID: "123456",
		Token: oauth2.Token{
			AccessToken:  "accessToken",
			RefreshToken: "refreshToken",
			Expiry:       expiryDate,
			TokenType:    "Bearer",
		},
	})
	assert.NilError(t, err)

	token, _ := azureLogin.GetValidToken()
	assert.Equal(t, token.AccessToken, "accessToken")
}

func TestInvalidLogin(t *testing.T) {
	m := &MockAzureHelper{}
	m.On("openAzureLoginPage", mock.AnythingOfType("string")).Run(func(args mock.Arguments) {
		redirectURL := args.Get(0).(string)
		err := queryKeyValue(redirectURL, "error", "access denied: login failed")
		assert.NilError(t, err)
	})

	azureLogin, err := testLoginService(t, m)
	assert.NilError(t, err)

	err = azureLogin.Login(context.TODO(), "")
	assert.Error(t, err, "no login code: login failed")
}

func TestValidLogin(t *testing.T) {
	var redirectURL string
	m := &MockAzureHelper{}
	m.On("openAzureLoginPage", mock.AnythingOfType("string")).Run(func(args mock.Arguments) {
		redirectURL = args.Get(0).(string)
		err := queryKeyValue(redirectURL, "code", "123456879")
		assert.NilError(t, err)
	})

	m.On("queryToken", mock.MatchedBy(func(data url.Values) bool {
		//Need a matcher here because the value of redirectUrl is not known until executing openAzureLoginPage
		return reflect.DeepEqual(data, url.Values{
			"grant_type":   []string{"authorization_code"},
			"client_id":    []string{clientID},
			"code":         []string{"123456879"},
			"scope":        []string{scopes},
			"redirect_uri": []string{redirectURL},
		})
	}), "organizations").Return(azureToken{
		RefreshToken: "firstRefreshToken",
		AccessToken:  "firstAccessToken",
		ExpiresIn:    3600,
		Foci:         "1",
	}, nil)

	authBody := `{"value":[{"id":"/tenants/12345a7c-c56d-43e8-9549-dd230ce8a038","tenantId":"12345a7c-c56d-43e8-9549-dd230ce8a038"}]}`

	m.On("queryAuthorizationAPI", authorizationURL, "Bearer firstAccessToken").Return([]byte(authBody), 200, nil)
	data := refreshTokenData("firstRefreshToken")
	m.On("queryToken", data, "12345a7c-c56d-43e8-9549-dd230ce8a038").Return(azureToken{
		RefreshToken: "newRefreshToken",
		AccessToken:  "newAccessToken",
		ExpiresIn:    3600,
		Foci:         "1",
	}, nil)
	azureLogin, err := testLoginService(t, m)
	assert.NilError(t, err)

	err = azureLogin.Login(context.TODO(), "")
	assert.NilError(t, err)

	loginToken, err := azureLogin.tokenStore.readToken()
	assert.NilError(t, err)
	assert.Equal(t, loginToken.Token.AccessToken, "newAccessToken")
	assert.Equal(t, loginToken.Token.RefreshToken, "newRefreshToken")
	assert.Assert(t, time.Now().Add(3500*time.Second).Before(loginToken.Token.Expiry))
	assert.Equal(t, loginToken.TenantID, "12345a7c-c56d-43e8-9549-dd230ce8a038")
	assert.Equal(t, loginToken.Token.Type(), "Bearer")
}

func TestValidLoginRequestedTenant(t *testing.T) {
	var redirectURL string
	m := &MockAzureHelper{}
	m.On("openAzureLoginPage", mock.AnythingOfType("string")).Run(func(args mock.Arguments) {
		redirectURL = args.Get(0).(string)
		err := queryKeyValue(redirectURL, "code", "123456879")
		assert.NilError(t, err)
	})

	m.On("queryToken", mock.MatchedBy(func(data url.Values) bool {
		//Need a matcher here because the value of redirectUrl is not known until executing openAzureLoginPage
		return reflect.DeepEqual(data, url.Values{
			"grant_type":   []string{"authorization_code"},
			"client_id":    []string{clientID},
			"code":         []string{"123456879"},
			"scope":        []string{scopes},
			"redirect_uri": []string{redirectURL},
		})
	}), "organizations").Return(azureToken{
		RefreshToken: "firstRefreshToken",
		AccessToken:  "firstAccessToken",
		ExpiresIn:    3600,
		Foci:         "1",
	}, nil)

	authBody := `{"value":[{"id":"/tenants/00000000-c56d-43e8-9549-dd230ce8a038","tenantId":"00000000-c56d-43e8-9549-dd230ce8a038"},
						   {"id":"/tenants/12345a7c-c56d-43e8-9549-dd230ce8a038","tenantId":"12345a7c-c56d-43e8-9549-dd230ce8a038"}]}`

	m.On("queryAuthorizationAPI", authorizationURL, "Bearer firstAccessToken").Return([]byte(authBody), 200, nil)
	data := refreshTokenData("firstRefreshToken")
	m.On("queryToken", data, "12345a7c-c56d-43e8-9549-dd230ce8a038").Return(azureToken{
		RefreshToken: "newRefreshToken",
		AccessToken:  "newAccessToken",
		ExpiresIn:    3600,
		Foci:         "1",
	}, nil)
	azureLogin, err := testLoginService(t, m)
	assert.NilError(t, err)

	err = azureLogin.Login(context.TODO(), "12345a7c-c56d-43e8-9549-dd230ce8a038")
	assert.NilError(t, err)

	loginToken, err := azureLogin.tokenStore.readToken()
	assert.NilError(t, err)
	assert.Equal(t, loginToken.Token.AccessToken, "newAccessToken")
	assert.Equal(t, loginToken.Token.RefreshToken, "newRefreshToken")
	assert.Assert(t, time.Now().Add(3500*time.Second).Before(loginToken.Token.Expiry))
	assert.Equal(t, loginToken.TenantID, "12345a7c-c56d-43e8-9549-dd230ce8a038")
	assert.Equal(t, loginToken.Token.Type(), "Bearer")
}

func TestLoginNoTenant(t *testing.T) {
	var redirectURL string
	m := &MockAzureHelper{}
	m.On("openAzureLoginPage", mock.AnythingOfType("string")).Run(func(args mock.Arguments) {
		redirectURL = args.Get(0).(string)
		err := queryKeyValue(redirectURL, "code", "123456879")
		assert.NilError(t, err)
	})

	m.On("queryToken", mock.MatchedBy(func(data url.Values) bool {
		//Need a matcher here because the value of redirectUrl is not known until executing openAzureLoginPage
		return reflect.DeepEqual(data, url.Values{
			"grant_type":   []string{"authorization_code"},
			"client_id":    []string{clientID},
			"code":         []string{"123456879"},
			"scope":        []string{scopes},
			"redirect_uri": []string{redirectURL},
		})
	}), "organizations").Return(azureToken{
		RefreshToken: "firstRefreshToken",
		AccessToken:  "firstAccessToken",
		ExpiresIn:    3600,
		Foci:         "1",
	}, nil)

	authBody := `{"value":[{"id":"/tenants/12345a7c-c56d-43e8-9549-dd230ce8a038","tenantId":"12345a7c-c56d-43e8-9549-dd230ce8a038"}]}`
	m.On("queryAuthorizationAPI", authorizationURL, "Bearer firstAccessToken").Return([]byte(authBody), 200, nil)

	azureLogin, err := testLoginService(t, m)
	assert.NilError(t, err)

	err = azureLogin.Login(context.TODO(), "00000000-c56d-43e8-9549-dd230ce8a038")
	assert.Error(t, err, "could not find requested azure tenant 00000000-c56d-43e8-9549-dd230ce8a038: login failed")
}

func TestLoginRequestedTenantNotFound(t *testing.T) {
	var redirectURL string
	m := &MockAzureHelper{}
	m.On("openAzureLoginPage", mock.AnythingOfType("string")).Run(func(args mock.Arguments) {
		redirectURL = args.Get(0).(string)
		err := queryKeyValue(redirectURL, "code", "123456879")
		assert.NilError(t, err)
	})

	m.On("queryToken", mock.MatchedBy(func(data url.Values) bool {
		//Need a matcher here because the value of redirectUrl is not known until executing openAzureLoginPage
		return reflect.DeepEqual(data, url.Values{
			"grant_type":   []string{"authorization_code"},
			"client_id":    []string{clientID},
			"code":         []string{"123456879"},
			"scope":        []string{scopes},
			"redirect_uri": []string{redirectURL},
		})
	}), "organizations").Return(azureToken{
		RefreshToken: "firstRefreshToken",
		AccessToken:  "firstAccessToken",
		ExpiresIn:    3600,
		Foci:         "1",
	}, nil)

	authBody := `{"value":[]}`
	m.On("queryAuthorizationAPI", authorizationURL, "Bearer firstAccessToken").Return([]byte(authBody), 200, nil)

	azureLogin, err := testLoginService(t, m)
	assert.NilError(t, err)

	err = azureLogin.Login(context.TODO(), "")
	assert.Error(t, err, "could not find azure tenant: login failed")
}

func TestLoginAuthorizationFailed(t *testing.T) {
	var redirectURL string
	m := &MockAzureHelper{}
	m.On("openAzureLoginPage", mock.AnythingOfType("string")).Run(func(args mock.Arguments) {
		redirectURL = args.Get(0).(string)
		err := queryKeyValue(redirectURL, "code", "123456879")
		assert.NilError(t, err)
	})

	m.On("queryToken", mock.MatchedBy(func(data url.Values) bool {
		//Need a matcher here because the value of redirectUrl is not known until executing openAzureLoginPage
		return reflect.DeepEqual(data, url.Values{
			"grant_type":   []string{"authorization_code"},
			"client_id":    []string{clientID},
			"code":         []string{"123456879"},
			"scope":        []string{scopes},
			"redirect_uri": []string{redirectURL},
		})
	}), "organizations").Return(azureToken{
		RefreshToken: "firstRefreshToken",
		AccessToken:  "firstAccessToken",
		ExpiresIn:    3600,
		Foci:         "1",
	}, nil)

	authBody := `[access denied]`

	m.On("queryAuthorizationAPI", authorizationURL, "Bearer firstAccessToken").Return([]byte(authBody), 400, nil)

	azureLogin, err := testLoginService(t, m)
	assert.NilError(t, err)

	err = azureLogin.Login(context.TODO(), "")
	assert.Error(t, err, "unable to login status code 400: [access denied]: login failed")
}

func refreshTokenData(refreshToken string) url.Values {
	return url.Values{
		"grant_type":    []string{"refresh_token"},
		"client_id":     []string{clientID},
		"scope":         []string{scopes},
		"refresh_token": []string{refreshToken},
	}
}

func queryKeyValue(redirectURL string, key string, value string) error {
	req, err := http.NewRequest("GET", redirectURL, nil)
	if err != nil {
		return err
	}
	q := req.URL.Query()
	q.Add(key, value)
	req.URL.RawQuery = q.Encode()
	client := &http.Client{}
	_, err = client.Do(req)
	return err
}

type MockAzureHelper struct {
	mock.Mock
}

func (s *MockAzureHelper) queryToken(data url.Values, tenantID string) (token azureToken, err error) {
	args := s.Called(data, tenantID)
	return args.Get(0).(azureToken), args.Error(1)
}

func (s *MockAzureHelper) queryAuthorizationAPI(authorizationURL string, authorizationHeader string) ([]byte, int, error) {
	args := s.Called(authorizationURL, authorizationHeader)
	return args.Get(0).([]byte), args.Int(1), args.Error(2)
}

func (s *MockAzureHelper) openAzureLoginPage(redirectURL string) error {
	s.Called(redirectURL)
	return nil
}