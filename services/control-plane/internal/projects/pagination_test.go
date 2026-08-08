package projects

import (
	"context"
	"testing"
)

func TestListPageUsesStableBoundedOrganizationCursor(t *testing.T) {
	fixture := newProjectGitFixture(t)
	for _, name := range []string{"Charlie", "alpha", "Echo", "bravo", "delta"} {
		fixture.createProject(t, name)
	}

	first, err := fixture.service.ListPage(
		context.Background(), fixture.principal, fixture.tenantID, fixture.organizationID,
		ProjectListQuery{Limit: 2},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Items) != 2 || first.NextCursor == nil || first.Items[0].Name != "alpha" || first.Items[1].Name != "bravo" {
		t.Fatalf("first Project page = %#v", first)
	}
	second, err := fixture.service.ListPage(
		context.Background(), fixture.principal, fixture.tenantID, fixture.organizationID,
		ProjectListQuery{Limit: 2, Cursor: *first.NextCursor},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Items) != 2 || second.NextCursor == nil || second.Items[0].Name != "Charlie" || second.Items[1].Name != "delta" {
		t.Fatalf("second Project page = %#v", second)
	}
	third, err := fixture.service.ListPage(
		context.Background(), fixture.principal, fixture.tenantID, fixture.organizationID,
		ProjectListQuery{Limit: 2, Cursor: *second.NextCursor},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(third.Items) != 1 || third.NextCursor != nil || third.Items[0].Name != "Echo" {
		t.Fatalf("third Project page = %#v", third)
	}

	_, err = fixture.service.ListPage(
		context.Background(), fixture.principal, fixture.tenantID, fixture.organizationID,
		ProjectListQuery{Limit: 2, Cursor: "not-a-cursor"},
	)
	assertProjectProblemCode(t, err, "invalid_project_cursor")
	_, err = fixture.service.ListPage(
		context.Background(), fixture.principal, fixture.tenantID, fixture.organizationID,
		ProjectListQuery{Limit: 201},
	)
	assertProjectProblemCode(t, err, "invalid_project_limit")
}
