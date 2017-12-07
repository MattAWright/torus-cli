package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"fmt"

	"github.com/manifoldco/torus-cli/apitypes"
	"github.com/manifoldco/torus-cli/identity"
)

// CredentialsClient provides access to unencrypted credentials for viewing,
// and encrypts credentials when setting.
type CredentialsClient struct {
	client *apiRoundTripper
}

// Search returns all credentials at the given pathexp in an undecrypted state
func (c *CredentialsClient) Search(ctx context.Context, pathexp string, teamIDs *[]identity.ID) ([]apitypes.CredentialEnvelope, error) {
	v := &url.Values{}
	v.Set("pathexp", pathexp)
	v.Set("skip-decryption", "true")

	if teamIDs != nil {
		for _, t := range *teamIDs {
			fmt.Println("Api.Credentials.Search() - Adding team ID", t.String())
			v.Add("team_id", t.String())
		}
	}

	return c.listWorker(ctx, v)
}

// Get returns all credentials at the given path.
func (c *CredentialsClient) Get(ctx context.Context, path string) ([]apitypes.CredentialEnvelope, error) {
	v := &url.Values{}
	v.Set("path", path)

	return c.listWorker(ctx, v)
}

func (c *CredentialsClient) listWorker(ctx context.Context, v *url.Values) ([]apitypes.CredentialEnvelope, error) {
	var resp []apitypes.CredentialResp
	err := c.client.DaemonRoundTrip(ctx, "GET", "/credentials", v, nil, &resp, nil)
	if err != nil {
		return nil, err
	}

	return createEnvelopesFromResp(resp)
}

// Create creates the given credential
func (c *CredentialsClient) Create(ctx context.Context, creds []*apitypes.CredentialEnvelope,
	progress ProgressFunc) ([]apitypes.CredentialEnvelope, error) {

	resp := []apitypes.CredentialResp{}
	err := c.client.DaemonRoundTrip(ctx, "POST", "/credentials", nil, creds, &resp, progress)
	if err != nil {
		return nil, err
	}

	return createEnvelopesFromResp(resp)
}

func createEnvelopesFromResp(resp []apitypes.CredentialResp) ([]apitypes.CredentialEnvelope, error) {
	results := []apitypes.CredentialEnvelope{}

	for _, c := range resp {
		var cBody apitypes.Credential
		switch c.Version {
		case 1:
			cBodyV1 := apitypes.BaseCredential{}
			err := json.Unmarshal(c.Body, &cBodyV1)
			if err != nil {
				return nil, err
			}

			cBody = &cBodyV1
		case 2:
			cBodyV2 := apitypes.CredentialV2{}
			err := json.Unmarshal(c.Body, &cBodyV2)
			if err != nil {
				return nil, err
			}

			cBody = &cBodyV2
		default:
			return nil, errors.New("Unknown credential version")
		}

		results = append(results, apitypes.CredentialEnvelope{
			ID:      c.ID,
			Version: c.Version,
			Body:    &cBody,
		})
	}

	return results, nil
}
