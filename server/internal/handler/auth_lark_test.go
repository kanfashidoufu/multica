package handler

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"

	enterpriseLark "github.com/multica-ai/multica/server/internal/enterprise/lark"
)

func TestCompleteLarkLoginOnboardingMarksNewLarkUserOnboarded(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}

	ctx := context.Background()
	const (
		email     = "lark-onboard-new@multica.test"
		tenantKey = "tenant-onboard-new"
		openID    = "ou_onboard_new"
		unionID   = "onboard-new-union"
	)
	t.Cleanup(func() {
		_, _ = testPool.Exec(ctx, `DELETE FROM "user" WHERE email = $1`, email)
	})
	_, _ = testPool.Exec(ctx, `DELETE FROM "user" WHERE email = $1`, email)

	req := httptest.NewRequest("POST", "/auth/lark", nil)
	user, isNew, err := testHandler.findOrCreateLarkUser(req, enterpriseLark.Profile{
		OpenID:    openID,
		UnionID:   unionID,
		TenantKey: tenantKey,
		Email:     email,
		Name:      "Lark Onboard New",
		Raw:       []byte(`{"source":"test"}`),
	})
	if err != nil {
		t.Fatalf("findOrCreateLarkUser: %v", err)
	}
	if !isNew {
		t.Fatal("expected a fresh Lark login user")
	}
	if user.OnboardedAt.Valid {
		t.Fatal("findOrCreateLarkUser should not mark onboarding by itself")
	}

	user, err = testHandler.completeLarkLoginOnboarding(req, user)
	if err != nil {
		t.Fatalf("completeLarkLoginOnboarding: %v", err)
	}
	if !user.OnboardedAt.Valid {
		t.Fatal("expected Lark login to mark onboarded_at")
	}

	var onboardedAt *time.Time
	if err := testPool.QueryRow(ctx,
		`SELECT onboarded_at FROM "user" WHERE id = $1`,
		user.ID,
	).Scan(&onboardedAt); err != nil {
		t.Fatalf("lookup user onboarding state: %v", err)
	}
	if onboardedAt == nil {
		t.Fatal("expected onboarded_at to be persisted")
	}
}
