package externalissue

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	enterpriseLark "github.com/multica-ai/multica/server/internal/enterprise/lark"
)

var directoryID = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,128}$`)

const syndraBugV2 = "syndra.multica.version_bug.webhook.v2"

// V2 uses the decimal representation of a BIGINT, never the legacy mate ID.
func directoryIdentity(schema string, person BugSyncPerson) (int64, bool) {
	if schema == syndraBugV2 {
		id, err := strconv.ParseInt(person.SyndraUserID, 10, 64)
		return id, err == nil && id > 0 && strconv.FormatInt(id, 10) == person.SyndraUserID
	}
	if person.MateID != nil && *person.MateID > 0 {
		return *person.MateID, true
	}
	return 0, false
}

// These are the Syndra web client's deployment API bases. Never use a
// webhook-provided source_url as a service-to-service destination.
func syndraDirectoryURL(env string) string {
	return syndraBugEndpoint(env, "assignee-identity")
}

func syndraBugEndpoint(env, operation string) string {
	path := "/open/multica/version-bugs/" + operation
	switch env {
	case "prod":
		return "https://syndra.lggj.work/api" + path
	case "test":
		return "https://test-syndra.lggj.work/api" + path
	case "dev":
		return "https://dev-syndra.lggj.work/api" + path
	case "pre":
		return "https://pre-api.qingx.cn" + path
	default:
		return ""
	}
}

// Pin the proof to this lookup and direction. The source environment never
// receives a reusable Multica webhook token, even when its secret differs.
func syndraDirectoryProof(secret, direction, env string, bug, mate int64, extra ...string) string {
	return signedDirectoryProof("v1", secret, direction, env, bug, mate, extra...)
}

func signedDirectoryProof(version, secret, direction, env string, bug, id int64, extra ...string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	fields := []string{"multica:syndra:bug-assignee:" + version, direction, env, strconv.FormatInt(bug, 10), strconv.FormatInt(id, 10)}
	mac.Write([]byte(strings.Join(append(fields, extra...), "\x00")))
	return hex.EncodeToString(mac.Sum(nil))
}

// ResolveBugDirectoryProfile trusts neither a person supplied by the webhook
// nor an open_id obtained with the Syndra app. Re-read the actual assignment and
// fetch the tenant-scoped employee using the same app that handles Multica login.
func (i *Importer) ResolveBugDirectoryProfile(ctx context.Context, payload BugSyncPayload, item BugSyncItem) (enterpriseLark.Profile, error) {
	var empty enterpriseLark.Profile
	endpoint := syndraDirectoryURL(payload.SourceEnv)
	secret := strings.TrimSpace(i.Config.WebhookToken)
	if endpoint == "" || secret == "" {
		return empty, errors.New("unrecognized Syndra environment or missing sync credentials")
	}
	person := bugAssigneePerson(item)
	id, ok := directoryIdentity(payload.SchemaVersion, person)
	if !ok {
		return empty, errors.New("missing Syndra assignee ID")
	}
	fields := map[string]any{"bug_id": item.BugID, "source_env": payload.SourceEnv}
	version := "v1"
	if payload.SchemaVersion == syndraBugV2 {
		version = "v2"
		fields["syndra_user_id"] = person.SyndraUserID
	} else {
		fields["mate_id"] = id
	}
	body, err := json.Marshal(fields)
	if err != nil {
		return empty, err
	}
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return empty, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+signedDirectoryProof(version, secret, "request", payload.SourceEnv, item.BugID, id))
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	if i.HTTPClient != nil {
		client.Transport = i.HTTPClient.Transport
	}
	response, err := client.Do(req)
	if err != nil {
		return empty, errors.New("Syndra assignee lookup unavailable")
	}
	defer response.Body.Close()
	var reply struct {
		Success bool
		Code    int
		Data    struct {
			Tenant       string `json:"tenant_key"`
			ID           string `json:"user_id"`
			Name         string
			Env          string `json:"source_env"`
			Bug          int64  `json:"bug_id"`
			Mate         int64  `json:"mate_id"`
			SyndraUserID string `json:"syndra_user_id"`
		}
	}
	if response.StatusCode != 200 || json.NewDecoder(io.LimitReader(response.Body, 1<<16)).Decode(&reply) != nil ||
		!reply.Success || reply.Code != 4000 || reply.Data.Tenant == "" || reply.Data.Env != payload.SourceEnv ||
		reply.Data.Bug != item.BugID || reply.Data.Name != bugPersonName(person) ||
		(version == "v2" && reply.Data.SyndraUserID != person.SyndraUserID) || (version == "v1" && reply.Data.Mate != id) ||
		!directoryID.MatchString(reply.Data.ID) {
		return empty, errors.New("Syndra did not verify the current assignee")
	}
	tenant := reply.Data.Tenant
	proof := signedDirectoryProof(version, secret, "response", payload.SourceEnv, item.BugID, id, tenant, reply.Data.ID, reply.Data.Name)
	if !hmac.Equal([]byte(response.Header.Get("X-Multica-Directory-Proof")), []byte(proof)) {
		return empty, errors.New("unverified Syndra directory response")
	}
	token, err := i.larkAppTenantAccessToken(ctx)
	if err != nil {
		return empty, err
	}
	var app struct {
		Code *int
		Data struct {
			Tenant struct {
				Key string `json:"tenant_key"`
			}
		}
	}
	if err := i.doLarkAppJSON(ctx, http.MethodGet, "/open-apis/tenant/v2/tenant/query", token, nil, &app); err != nil {
		return empty, err
	}
	if app.Code == nil || *app.Code != 0 || app.Data.Tenant.Key != tenant {
		return empty, errors.New("login application's tenant does not match Syndra")
	}
	var contact struct {
		Code *int
		Data struct {
			User struct {
				UserID          string `json:"user_id"`
				OpenID          string `json:"open_id"`
				UnionID         string `json:"union_id"`
				EnterpriseEmail string `json:"enterprise_email"`
				Name            string
				Status          struct {
					Active   *bool `json:"is_activated"`
					Frozen   *bool `json:"is_frozen"`
					Resigned *bool `json:"is_resigned"`
					Exited   bool  `json:"is_exited"`
					Unjoined bool  `json:"is_unjoin"`
				}
			}
		}
	}
	if err := i.doLarkAppJSON(ctx, http.MethodGet, "/open-apis/contact/v3/users/"+reply.Data.ID+"?user_id_type=user_id", token, nil, &contact); err != nil {
		return empty, err
	}
	user := contact.Data.User
	if contact.Code == nil || *contact.Code != 0 || user.UserID != reply.Data.ID || !directoryID.MatchString(user.OpenID) ||
		user.Name != reply.Data.Name || user.Status.Active == nil || !*user.Status.Active || user.Status.Resigned == nil || *user.Status.Resigned ||
		user.Status.Frozen == nil || *user.Status.Frozen || user.Status.Exited || user.Status.Unjoined {
		return empty, errors.New("Feishu did not verify an active matching employee")
	}
	return enterpriseLark.Profile{TenantKey: tenant, ExternalUserID: user.UserID, OpenID: user.OpenID, UnionID: user.UnionID,
		Name: user.Name, Email: strings.ToLower(strings.TrimSpace(user.EnterpriseEmail))}, nil
}
