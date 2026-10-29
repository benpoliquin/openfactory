package store

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"openfactory/internal/config"
	"openfactory/internal/model"
)

func TestIntegrationStatusIdentifiesReleaseOnlyAliasContention(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	repo := filepath.Join(t.TempDir(), "repository")
	if err := os.Mkdir(repo, 0700); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(t.TempDir(), "repository-alias")
	if err := os.Symlink(repo, alias); err != nil {
		t.Fatal(err)
	}
	firstProject := model.Project{Repository: repo, Branch: "alpha"}
	secondProject := model.Project{Repository: alias, Branch: "beta"}
	firstResources, err := config.IntegrationResources(firstProject, "staging")
	if err != nil {
		t.Fatal(err)
	}
	secondResources, err := config.IntegrationResources(secondProject, "staging")
	if err != nil {
		t.Fatal(err)
	}
	first := lane(t, s, "release-owner")
	second := lane(t, s, "release-waiter")
	if err = s.EnqueueIntegration(ctx, first, firstResources); err != nil {
		t.Fatal(err)
	}
	if err = s.EnqueueIntegration(ctx, second, secondResources); err != nil {
		t.Fatal(err)
	}
	if _, err = s.AcquireIntegration(ctx, first.ID, "policy"); err != nil {
		t.Fatal(err)
	}
	if _, err = s.AcquireIntegration(ctx, second.ID, "policy"); !errors.Is(err, ErrWaiting) {
		t.Fatalf("shared release resource not excluded: %v", err)
	}
	status, err := s.IntegrationStatus(ctx, second.ID)
	if err != nil {
		t.Fatal(err)
	}
	var release string
	for _, resource := range secondResources {
		if strings.HasPrefix(resource, "release:") {
			release = resource
		}
	}
	if release == "" || status.ContendedResource != release || status.OwnerAttemptID != first.ID || status.Position != 2 {
		t.Fatalf("wrong actual blocker for differing Git branches: %+v, want %s", status, release)
	}
	if status.ContendedResource == status.Resources[0] {
		t.Fatal("fixture must distinguish actual release contention from first Git resource")
	}
	cfg := model.Config{PolicyHash: "policy", MaxConcurrent: 8, MaxAttempts: 4, Projects: map[string]model.Project{"demo": secondProject}}
	public, err := s.FeatureStatus(ctx, second.FeatureID, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if public.Integration.ContendedResource != reportResource(release) || strings.Contains(public.Integration.ContendedResource, repo) {
		t.Fatalf("public contention resource lost identity or disclosed canonical path: %+v", public.Integration)
	}
}

func TestIntegrationStatusUsesEarliestQueuedSharedResource(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	owner := lane(t, s, "branch-owner")
	queued := lane(t, s, "release-predecessor")
	waiter := lane(t, s, "release-successor")
	for _, request := range []struct {
		attempt   model.Attempt
		resources []string
	}{
		{owner, []string{"git:repo:refs/heads/alpha"}},
		{queued, []string{"git:repo:refs/heads/alpha", "release:repo:production"}},
		{waiter, []string{"git:repo:refs/heads/beta", "release:repo:production"}},
	} {
		if err := s.EnqueueIntegration(ctx, request.attempt, request.resources); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.AcquireIntegration(ctx, owner.ID, "policy"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AcquireIntegration(ctx, queued.ID, "policy"); !errors.Is(err, ErrWaiting) {
		t.Fatalf("predecessor not queued: %v", err)
	}
	if _, err := s.AcquireIntegration(ctx, waiter.ID, "policy"); !errors.Is(err, ErrWaiting) {
		t.Fatalf("FIFO bypassed predecessor: %v", err)
	}
	status, err := s.IntegrationStatus(ctx, waiter.ID)
	if err != nil {
		t.Fatal(err)
	}
	if status.ContendedResource != "release:repo:production" || status.OwnerAttemptID != "" || status.Position != 2 {
		t.Fatalf("queued predecessor misreported as Git owner: %+v", status)
	}
}
