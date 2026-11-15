package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	gcs "github.com/scttfrdmn/globus-gcs-go"
)

// httpClient bounds Globus API calls (#48).
var httpClient = &http.Client{Timeout: 30 * time.Second}

// getSecretValue fetches a plaintext secret from Secrets Manager.
func getSecretValue(ctx context.Context, smClient *secretsmanager.Client, secretID string) (string, error) {
	out, err := smClient.GetSecretValue(ctx, &secretsmanager.GetSecretValueInput{
		SecretId: aws.String(secretID),
	})
	if err != nil {
		return "", err
	}
	return aws.ToString(out.SecretString), nil
}

// globusClientCredentialsToken obtains a Globus access token using the
// OAuth2 client credentials grant.
func globusClientCredentialsToken(clientID, clientSecret, scope string) (string, error) {
	form := url.Values{
		"grant_type": {"client_credentials"},
		"scope":      {scope},
	}
	req, err := http.NewRequest(http.MethodPost,
		"https://auth.globus.org/v2/oauth2/token",
		strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.SetBasicAuth(clientID, clientSecret)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("token request: %w", err)
	}
	defer resp.Body.Close()

	var tok struct {
		AccessToken string `json:"access_token"`
		Error       string `json:"error"`
		ErrorDesc   string `json:"error_description"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&tok); err != nil {
		return "", fmt.Errorf("parse token response: %w", err)
	}
	if tok.Error != "" {
		return "", fmt.Errorf("globus auth: %s: %s", tok.Error, tok.ErrorDesc)
	}
	return tok.AccessToken, nil
}

// gcsBaseURLFromTransfer looks up the GCS Manager API base URL for an endpoint
// using the Globus Transfer API's endpoint metadata.
func gcsBaseURLFromTransfer(ctx context.Context, endpointID, transferToken string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		"https://transfer.api.globus.org/v0.10/endpoint/"+endpointID, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+transferToken)

	resp, err := httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("transfer API: %w", err)
	}
	defer resp.Body.Close()

	var ep struct {
		HTTPSServer string `json:"https_server"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&ep); err != nil {
		return "", fmt.Errorf("parse endpoint response: %w", err)
	}
	if ep.HTTPSServer == "" {
		return "", fmt.Errorf("endpoint %s has no https_server field (GCS v5 required)", endpointID)
	}
	return ep.HTTPSServer, nil
}

// newGCSClient builds an authenticated GCS Manager API client for the current
// region's endpoint. It fetches Globus client credentials from Secrets Manager,
// discovers the endpoint domain via the Transfer API, and obtains a
// manage_collections-scoped access token.
func newGCSClient(ctx context.Context, smClient *secretsmanager.Client, ssmClient *ssm.Client) (*gcs.Client, error) {
	pfx := secretsPrefix()

	clientID, err := getSecretValue(ctx, smClient, pfx+"/globus-client-id")
	if err != nil {
		return nil, fmt.Errorf("get client ID: %w", err)
	}
	clientSecret, err := getSecretValue(ctx, smClient, pfx+"/globus-client-secret")
	if err != nil {
		return nil, fmt.Errorf("get client secret: %w", err)
	}
	endpointID, err := getSSMParam(ctx, ssmClient, endpointIDParamPath())
	if err != nil {
		return nil, fmt.Errorf("get endpoint ID: %w", err)
	}

	// Discover the GCS Manager domain via the Transfer API.
	transferToken, err := globusClientCredentialsToken(clientID, clientSecret,
		"urn:globus:auth:scope:transfer.api.globus.org:all")
	if err != nil {
		return nil, fmt.Errorf("get transfer token: %w", err)
	}
	baseURL, err := gcsBaseURLFromTransfer(ctx, endpointID, transferToken)
	if err != nil {
		return nil, fmt.Errorf("discover endpoint domain: %w", err)
	}

	// Get a manage_collections-scoped token for the GCS Manager API.
	gcsToken, err := globusClientCredentialsToken(clientID, clientSecret,
		gcs.ManageCollectionsScope(endpointID))
	if err != nil {
		return nil, fmt.Errorf("get GCS token: %w", err)
	}

	return gcs.NewClient(baseURL, &gcs.StaticAuthorizer{Token: gcsToken})
}
