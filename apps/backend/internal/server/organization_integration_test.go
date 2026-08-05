package server_test

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// jwtSubject extracts the "sub" claim from a JWT without verifying its
// signature — sufficient here since the token was just issued by the same
// test server and its authenticity isn't what's under test.
func jwtSubject(t *testing.T, token string) string {
	t.Helper()

	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatalf("malformed jwt: %q", token)
	}

	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatalf("decode jwt payload: %v", err)
	}

	var claims struct {
		Sub string `json:"sub"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		t.Fatalf("unmarshal jwt claims: %v", err)
	}
	if claims.Sub == "" {
		t.Fatalf("jwt missing sub claim: %s", payload)
	}
	return claims.Sub
}

// registeredUser is the outcome of a successful POST /auth/register.
type registeredUser struct {
	Email       string
	AccessToken string
	UserID      string
}

func registerUser(t *testing.T, client *http.Client, baseURL, prefix string) registeredUser {
	t.Helper()

	email := uniqueEmail(prefix)
	resp, body := doJSON(t, client, baseURL, http.MethodPost, "/auth/register",
		map[string]any{"email": email, "password": "password123"}, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("register %s: status = %d, want 200; body = %v", prefix, resp.StatusCode, body)
	}

	accessToken, _ := body["accessToken"].(string)
	if accessToken == "" {
		t.Fatalf("register %s: missing accessToken: %v", prefix, body)
	}

	return registeredUser{Email: email, AccessToken: accessToken, UserID: jwtSubject(t, accessToken)}
}

func uniqueSlug(prefix string) string {
	return prefix + "-" + uuid.NewString()
}

// createdOrg is the outcome of a successful POST /organizations, plus the
// owner's own credentials for use as the caller in later requests.
type createdOrg struct {
	ID    string
	Owner registeredUser
}

func createOrgWithOwner(t *testing.T, client *http.Client, baseURL, slugPrefix string) createdOrg {
	t.Helper()

	owner := registerUser(t, client, baseURL, slugPrefix+"-owner")

	resp, body := doJSON(t, client, baseURL, http.MethodPost, "/organizations",
		map[string]any{"name": "Test Org", "slug": uniqueSlug(slugPrefix)},
		map[string]string{"Authorization": "Bearer " + owner.AccessToken})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("create org: status = %d, want 200; body = %v", resp.StatusCode, body)
	}

	orgID, _ := body["id"].(string)
	if orgID == "" {
		t.Fatalf("create org: missing id: %v", body)
	}

	return createdOrg{ID: orgID, Owner: owner}
}

// inviteMember invites email into org with role, using org's owner as the
// caller, and fails the test unless the invite succeeds.
func inviteMember(t *testing.T, client *http.Client, baseURL string, org createdOrg, email, role string) {
	t.Helper()

	resp, body := doJSON(t, client, baseURL, http.MethodPost, "/organizations/invite",
		map[string]any{"email": email, "role": role},
		map[string]string{
			"Authorization":     "Bearer " + org.Owner.AccessToken,
			"x-organization-id": org.ID,
		})
	if resp.StatusCode != http.StatusOK || body["success"] != true {
		t.Fatalf("invite %s: status = %d, body = %v", email, resp.StatusCode, body)
	}
}

func TestIntegration_OrganizationsCreate(t *testing.T) {
	ts, _, _ := setupTestServer(t)
	client := ts.Client()

	t.Run("happy path", func(t *testing.T) {
		owner := registerUser(t, client, ts.URL, "org-create-happy")
		slug := uniqueSlug("acme-corp")

		resp, body := doJSON(t, client, ts.URL, http.MethodPost, "/organizations",
			map[string]any{"name": "Acme Corp", "slug": slug},
			map[string]string{"Authorization": "Bearer " + owner.AccessToken})
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200; body = %v", resp.StatusCode, body)
		}
		if body["name"] != "Acme Corp" {
			t.Errorf("name = %v, want %q", body["name"], "Acme Corp")
		}
		if body["slug"] != slug {
			t.Errorf("slug = %v, want %q", body["slug"], slug)
		}
		if _, ok := body["id"].(string); !ok || body["id"] == "" {
			t.Errorf("missing/empty id: %v", body)
		}
		if _, ok := body["createdAt"].(string); !ok {
			t.Errorf("missing createdAt: %v", body)
		}
		if _, ok := body["updatedAt"].(string); !ok {
			t.Errorf("missing updatedAt: %v", body)
		}
	})

	t.Run("duplicate slug", func(t *testing.T) {
		owner := registerUser(t, client, ts.URL, "org-create-dup")
		slug := uniqueSlug("dup-org")

		doJSON(t, client, ts.URL, http.MethodPost, "/organizations",
			map[string]any{"name": "First", "slug": slug},
			map[string]string{"Authorization": "Bearer " + owner.AccessToken})

		resp, body := doJSON(t, client, ts.URL, http.MethodPost, "/organizations",
			map[string]any{"name": "Second", "slug": slug},
			map[string]string{"Authorization": "Bearer " + owner.AccessToken})
		if resp.StatusCode != http.StatusConflict {
			t.Fatalf("status = %d, want 409; body = %v", resp.StatusCode, body)
		}
		if body["message"] != "Organization slug already taken" {
			t.Fatalf("message = %v, want %q", body["message"], "Organization slug already taken")
		}
	})

	t.Run("invalid slug", func(t *testing.T) {
		owner := registerUser(t, client, ts.URL, "org-create-invalid")

		resp, body := doJSON(t, client, ts.URL, http.MethodPost, "/organizations",
			map[string]any{"name": "Bad Slug Org", "slug": "Not_A_Valid-Slug"},
			map[string]string{"Authorization": "Bearer " + owner.AccessToken})
		if resp.StatusCode != http.StatusUnprocessableEntity {
			t.Fatalf("status = %d, want 422; body = %v", resp.StatusCode, body)
		}
		if body["message"] != "Validation failed" {
			t.Fatalf("message = %v, want %q", body["message"], "Validation failed")
		}
	})

	t.Run("unauthenticated", func(t *testing.T) {
		resp, body := doJSON(t, client, ts.URL, http.MethodPost, "/organizations",
			map[string]any{"name": "No Auth", "slug": uniqueSlug("no-auth")}, nil)
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401; body = %v", resp.StatusCode, body)
		}
		if body["message"] != "Unauthorized" {
			t.Fatalf("message = %v, want %q", body["message"], "Unauthorized")
		}
	})
}

func TestIntegration_OrganizationsList(t *testing.T) {
	ts, _, _ := setupTestServer(t)
	client := ts.Client()

	t.Run("with memberships", func(t *testing.T) {
		org := createOrgWithOwner(t, client, ts.URL, "org-list")

		req, err := http.NewRequest(http.MethodGet, ts.URL+"/organizations", nil)
		if err != nil {
			t.Fatalf("new request: %v", err)
		}
		req.Header.Set("Authorization", "Bearer "+org.Owner.AccessToken)

		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("do request: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200", resp.StatusCode)
		}

		var memberships []map[string]any
		if err := json.NewDecoder(resp.Body).Decode(&memberships); err != nil {
			t.Fatalf("decode response: %v", err)
		}

		var found map[string]any
		for _, m := range memberships {
			if m["organizationId"] == org.ID {
				found = m
				break
			}
		}
		if found == nil {
			t.Fatalf("expected a membership for org %s, got %v", org.ID, memberships)
		}
		if found["role"] != "owner" {
			t.Errorf("role = %v, want %q", found["role"], "owner")
		}

		embedded, ok := found["organization"].(map[string]any)
		if !ok {
			t.Fatalf("expected embedded organization object, got %v", found["organization"])
		}
		if embedded["id"] != org.ID {
			t.Errorf("embedded organization id = %v, want %q", embedded["id"], org.ID)
		}
	})

	t.Run("no memberships serializes as an empty array, not null", func(t *testing.T) {
		user := registerUser(t, client, ts.URL, "org-list-empty")

		req, err := http.NewRequest(http.MethodGet, ts.URL+"/organizations", nil)
		if err != nil {
			t.Fatalf("new request: %v", err)
		}
		req.Header.Set("Authorization", "Bearer "+user.AccessToken)

		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("do request: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200", resp.StatusCode)
		}

		raw, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatalf("read body: %v", err)
		}
		if got := strings.TrimSpace(string(raw)); got != "[]" {
			t.Fatalf("body = %q, want %q (a JSON array, not null)", got, "[]")
		}
	})
}

func TestIntegration_OrganizationsInvite(t *testing.T) {
	ts, _, _ := setupTestServer(t)
	client := ts.Client()

	t.Run("happy path", func(t *testing.T) {
		org := createOrgWithOwner(t, client, ts.URL, "org-invite-happy")
		invitee := registerUser(t, client, ts.URL, "org-invite-happy-invitee")

		resp, body := doJSON(t, client, ts.URL, http.MethodPost, "/organizations/invite",
			map[string]any{"email": invitee.Email, "role": "admin"},
			map[string]string{
				"Authorization":     "Bearer " + org.Owner.AccessToken,
				"x-organization-id": org.ID,
			})
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200; body = %v", resp.StatusCode, body)
		}
		if body["success"] != true {
			t.Fatalf("success = %v, want true", body["success"])
		}

		// invitee should now show up in the org's own membership list.
		listReq, err := http.NewRequest(http.MethodGet, ts.URL+"/organizations", nil)
		if err != nil {
			t.Fatalf("new request: %v", err)
		}
		listReq.Header.Set("Authorization", "Bearer "+invitee.AccessToken)
		listResp, err := client.Do(listReq)
		if err != nil {
			t.Fatalf("do request: %v", err)
		}
		defer listResp.Body.Close()
		var memberships []map[string]any
		if err := json.NewDecoder(listResp.Body).Decode(&memberships); err != nil {
			t.Fatalf("decode response: %v", err)
		}
		var joined bool
		for _, m := range memberships {
			if m["organizationId"] == org.ID && m["role"] == "admin" {
				joined = true
			}
		}
		if !joined {
			t.Fatalf("expected invitee to have an admin membership in org %s, got %v", org.ID, memberships)
		}
	})

	t.Run("missing x-organization-id header", func(t *testing.T) {
		org := createOrgWithOwner(t, client, ts.URL, "org-invite-noheader")

		resp, body := doJSON(t, client, ts.URL, http.MethodPost, "/organizations/invite",
			map[string]any{"email": "someone@example.com", "role": "member"},
			map[string]string{"Authorization": "Bearer " + org.Owner.AccessToken})
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400; body = %v", resp.StatusCode, body)
		}
		if body["message"] != "Missing x-organization-id header" {
			t.Fatalf("message = %v, want %q", body["message"], "Missing x-organization-id header")
		}
	})

	t.Run("not a member of the target org", func(t *testing.T) {
		org := createOrgWithOwner(t, client, ts.URL, "org-invite-notmember")
		outsider := registerUser(t, client, ts.URL, "org-invite-outsider")

		resp, body := doJSON(t, client, ts.URL, http.MethodPost, "/organizations/invite",
			map[string]any{"email": "someone@example.com", "role": "member"},
			map[string]string{
				"Authorization":     "Bearer " + outsider.AccessToken,
				"x-organization-id": org.ID,
			})
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("status = %d, want 403; body = %v", resp.StatusCode, body)
		}
		if body["message"] != "Not a member of this organization" {
			t.Fatalf("message = %v, want %q", body["message"], "Not a member of this organization")
		}
	})

	t.Run("user not found", func(t *testing.T) {
		org := createOrgWithOwner(t, client, ts.URL, "org-invite-usernotfound")

		resp, body := doJSON(t, client, ts.URL, http.MethodPost, "/organizations/invite",
			map[string]any{"email": uniqueEmail("nobody"), "role": "member"},
			map[string]string{
				"Authorization":     "Bearer " + org.Owner.AccessToken,
				"x-organization-id": org.ID,
			})
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("status = %d, want 404; body = %v", resp.StatusCode, body)
		}
		if body["message"] != "User not found" {
			t.Fatalf("message = %v, want %q", body["message"], "User not found")
		}
	})

	t.Run("already a member", func(t *testing.T) {
		org := createOrgWithOwner(t, client, ts.URL, "org-invite-already")
		invitee := registerUser(t, client, ts.URL, "org-invite-already-invitee")

		resp1, body1 := doJSON(t, client, ts.URL, http.MethodPost, "/organizations/invite",
			map[string]any{"email": invitee.Email, "role": "member"},
			map[string]string{
				"Authorization":     "Bearer " + org.Owner.AccessToken,
				"x-organization-id": org.ID,
			})
		if resp1.StatusCode != http.StatusOK || body1["success"] != true {
			t.Fatalf("first invite: status = %d, body = %v", resp1.StatusCode, body1)
		}

		resp2, body2 := doJSON(t, client, ts.URL, http.MethodPost, "/organizations/invite",
			map[string]any{"email": invitee.Email, "role": "member"},
			map[string]string{
				"Authorization":     "Bearer " + org.Owner.AccessToken,
				"x-organization-id": org.ID,
			})
		if resp2.StatusCode != http.StatusConflict {
			t.Fatalf("second invite: status = %d, want 409; body = %v", resp2.StatusCode, body2)
		}
		if body2["message"] != "User is already a member" {
			t.Fatalf("second invite: message = %v, want %q", body2["message"], "User is already a member")
		}
	})

	t.Run("member role cannot invite", func(t *testing.T) {
		org := createOrgWithOwner(t, client, ts.URL, "org-invite-forbidden")
		member := registerUser(t, client, ts.URL, "org-invite-forbidden-member")
		inviteMember(t, client, ts.URL, org, member.Email, "member")

		resp, body := doJSON(t, client, ts.URL, http.MethodPost, "/organizations/invite",
			map[string]any{"email": uniqueEmail("org-invite-forbidden-target"), "role": "member"},
			map[string]string{
				"Authorization":     "Bearer " + member.AccessToken,
				"x-organization-id": org.ID,
			})
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("status = %d, want 403; body = %v", resp.StatusCode, body)
		}
		if body["message"] != "Insufficient permissions" {
			t.Fatalf("message = %v, want %q", body["message"], "Insufficient permissions")
		}
	})
}

func TestIntegration_OrganizationsRemoveMember(t *testing.T) {
	ts, _, _ := setupTestServer(t)
	client := ts.Client()

	t.Run("happy path", func(t *testing.T) {
		org := createOrgWithOwner(t, client, ts.URL, "org-remove-happy")
		member := registerUser(t, client, ts.URL, "org-remove-happy-member")
		inviteMember(t, client, ts.URL, org, member.Email, "member")

		resp, body := doJSON(t, client, ts.URL, http.MethodDelete, "/organizations/members/"+member.UserID,
			nil,
			map[string]string{
				"Authorization":     "Bearer " + org.Owner.AccessToken,
				"x-organization-id": org.ID,
			})
		if resp.StatusCode != http.StatusOK || body["success"] != true {
			t.Fatalf("remove member: status = %d, body = %v", resp.StatusCode, body)
		}
	})

	t.Run("cannot remove owner", func(t *testing.T) {
		org := createOrgWithOwner(t, client, ts.URL, "org-remove-owner")

		resp, body := doJSON(t, client, ts.URL, http.MethodDelete, "/organizations/members/"+org.Owner.UserID,
			nil,
			map[string]string{
				"Authorization":     "Bearer " + org.Owner.AccessToken,
				"x-organization-id": org.ID,
			})
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("status = %d, want 403; body = %v", resp.StatusCode, body)
		}
		if body["message"] != "Cannot remove organization owner" {
			t.Fatalf("message = %v, want %q", body["message"], "Cannot remove organization owner")
		}
	})

	t.Run("member not found", func(t *testing.T) {
		org := createOrgWithOwner(t, client, ts.URL, "org-remove-notfound")

		resp, body := doJSON(t, client, ts.URL, http.MethodDelete, "/organizations/members/"+uuid.NewString(),
			nil,
			map[string]string{
				"Authorization":     "Bearer " + org.Owner.AccessToken,
				"x-organization-id": org.ID,
			})
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("status = %d, want 404; body = %v", resp.StatusCode, body)
		}
		if body["message"] != "Member not found" {
			t.Fatalf("message = %v, want %q", body["message"], "Member not found")
		}
	})

	t.Run("member role cannot remove", func(t *testing.T) {
		org := createOrgWithOwner(t, client, ts.URL, "org-remove-forbidden")
		member := registerUser(t, client, ts.URL, "org-remove-forbidden-member")
		inviteMember(t, client, ts.URL, org, member.Email, "member")

		resp, body := doJSON(t, client, ts.URL, http.MethodDelete, "/organizations/members/"+member.UserID,
			nil,
			map[string]string{
				"Authorization":     "Bearer " + member.AccessToken,
				"x-organization-id": org.ID,
			})
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("status = %d, want 403; body = %v", resp.StatusCode, body)
		}
		if body["message"] != "Insufficient permissions" {
			t.Fatalf("message = %v, want %q", body["message"], "Insufficient permissions")
		}
	})
}
