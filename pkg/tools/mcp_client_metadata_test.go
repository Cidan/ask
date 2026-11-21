package tools

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// The client ID metadata document ask publishes names ask's identifier and the redirect its loopback
// callback uses, as an authorization server checks them: client_id must equal the URL it fetched.
func TestClientIDMetadataDocument(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "oauth", "client-metadata.json"))
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		ClientID                string   `json:"client_id"`
		ClientName              string   `json:"client_name"`
		RedirectURIs            []string `json:"redirect_uris"`
		GrantTypes              []string `json:"grant_types"`
		TokenEndpointAuthMethod string   `json:"token_endpoint_auth_method"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	if doc.ClientID != MCPClientIDMetadataDocumentURL {
		t.Errorf("client_id %q, want %q", doc.ClientID, MCPClientIDMetadataDocumentURL)
	}
	if doc.ClientName != "ask" || doc.TokenEndpointAuthMethod != "none" {
		t.Errorf("client_name %q, token_endpoint_auth_method %q", doc.ClientName, doc.TokenEndpointAuthMethod)
	}
	if !slices.Contains(doc.RedirectURIs, "http://127.0.0.1/callback") {
		t.Errorf("redirect_uris %v lack the loopback callback", doc.RedirectURIs)
	}
	if !slices.Contains(doc.GrantTypes, "refresh_token") {
		t.Errorf("grant_types %v lack refresh_token", doc.GrantTypes)
	}
	cb, err := newLazyOAuthCallback()
	if err != nil {
		t.Fatal(err)
	}
	if want := "http://127.0.0.1:"; len(cb.url) < len(want) || cb.url[:len(want)] != want || filepath.Base(cb.url) != "callback" {
		t.Errorf("the loopback callback %q is not the document's redirect on a port", cb.url)
	}
}
