package agentd

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/synara-ai/synara/services/control-plane/internal/executions"
)

func newReceiptProbeClient(t *testing.T, serverURL string) *Client {
	t.Helper()
	parsed, err := url.Parse(serverURL)
	if err != nil {
		t.Fatal(err)
	}
	return NewClient(Config{ControlPlaneURL: parsed, RequestTimeout: 5 * time.Second})
}

func receiptProbeServer(capture *[]string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*capture = append(*capture, r.Header.Get("X-Request-ID"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
}

// Start, Complete and Fail happen once per Execution Generation and the Control
// Plane makes them receipt-idempotent. The request ID therefore has to be
// derived from the Generation: with a random ID a lost response can never be
// replayed, so a Completion that already committed server-side comes back as an
// error and the Worker reports a terminal failure for finished work.
func TestExecutionLifecycleRequestIDsAreReplayable(t *testing.T) {
	executionID := uuid.New()
	lease := executions.Lease{TenantID: uuid.New(), Generation: 4, LeaseToken: "lease-token"}

	for _, testCase := range []struct {
		name string
		call func(*Client) error
	}{
		{"start", func(c *Client) error { return c.Start(context.Background(), executionID, lease) }},
		{"complete", func(c *Client) error {
			return c.Complete(context.Background(), executionID, lease, RunnerResult{})
		}},
		{"fail", func(c *Client) error {
			return c.Fail(context.Background(), executionID, lease, "runner_failed", "boom")
		}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			var ids []string
			server := receiptProbeServer(&ids)
			defer server.Close()
			client := newReceiptProbeClient(t, server.URL)

			if err := testCase.call(client); err != nil {
				t.Fatalf("first %s: %v", testCase.name, err)
			}
			// A retry after a lost response must present the same receipt key.
			if err := testCase.call(client); err != nil {
				t.Fatalf("retried %s: %v", testCase.name, err)
			}
			if len(ids) != 2 || ids[0] == "" {
				t.Fatalf("%s request IDs = %v", testCase.name, ids)
			}
			if ids[0] != ids[1] {
				t.Fatalf("%s is not replayable: request IDs differ %v", testCase.name, ids)
			}
		})
	}
}

// A different failure reason must not collide with a stored receipt, which the
// Control Plane would reject as request_id_reused.
func TestExecutionFailRequestIDVariesWithFailureCode(t *testing.T) {
	executionID := uuid.New()
	lease := executions.Lease{TenantID: uuid.New(), Generation: 2, LeaseToken: "lease-token"}
	var ids []string
	server := receiptProbeServer(&ids)
	defer server.Close()
	client := newReceiptProbeClient(t, server.URL)

	if err := client.Fail(context.Background(), executionID, lease, "runner_failed", "a"); err != nil {
		t.Fatal(err)
	}
	if err := client.Fail(context.Background(), executionID, lease, "workspace_failed", "b"); err != nil {
		t.Fatal(err)
	}
	if len(ids) != 2 || ids[0] == ids[1] {
		t.Fatalf("distinct failure codes reused one receipt key: %v", ids)
	}
}

// Renew runs repeatedly inside one Generation, so it must stay unique: a stable
// key would replay the first expiry and the lease would stop being extended.
func TestExecutionRenewUsesDistinctRequestIDs(t *testing.T) {
	executionID := uuid.New()
	lease := executions.Lease{TenantID: uuid.New(), Generation: 7, LeaseToken: "lease-token"}
	var ids []string
	server := receiptProbeServer(&ids)
	defer server.Close()
	client := newReceiptProbeClient(t, server.URL)

	for attempt := 0; attempt < 2; attempt++ {
		if _, err := client.Renew(context.Background(), executionID, lease); err != nil {
			t.Fatalf("renew %d: %v", attempt, err)
		}
	}
	if len(ids) != 2 || ids[0] == "" || ids[0] == ids[1] {
		t.Fatalf("renew request IDs = %v", ids)
	}
}
