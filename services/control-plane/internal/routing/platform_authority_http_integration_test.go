package routing

import (
	"bytes"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestPlatformAuthorityHTTPReplicaReplayIntegration(t *testing.T) {
	baseURL := strings.TrimRight(strings.TrimSpace(os.Getenv("SYNARA_TEST_PLATFORM_ROUTING_HTTP_URL")), "/")
	if baseURL == "" {
		t.Skip("SYNARA_TEST_PLATFORM_ROUTING_HTTP_URL is not configured")
	}
	targetID, err := uuid.Parse(strings.TrimSpace(os.Getenv("SYNARA_TEST_PLATFORM_ROUTING_TARGET_ID")))
	if err != nil || targetID == uuid.Nil {
		t.Fatal("SYNARA_TEST_PLATFORM_ROUTING_TARGET_ID must be a UUID")
	}
	seed, err := hex.DecodeString("9d61b19deffd5a60ba844af492ec2cc44449c5697b326919703bac031cae7f60")
	if err != nil {
		t.Fatal(err)
	}
	privateKey := ed25519.NewKeyFromSeed(seed)
	now := time.Now().UTC().Add(-time.Second).Truncate(time.Microsecond)
	if rawObservedAt := strings.TrimSpace(os.Getenv("SYNARA_TEST_PLATFORM_ROUTING_OBSERVED_AT")); rawObservedAt != "" {
		parsed, parseErr := time.Parse(time.RFC3339Nano, rawObservedAt)
		if parseErr != nil || parsed.Location() != time.UTC || parsed.Format(time.RFC3339Nano) != rawObservedAt {
			t.Fatal("SYNARA_TEST_PLATFORM_ROUTING_OBSERVED_AT must be canonical UTC RFC3339Nano")
		}
		now = parsed
	}
	nonce := uuid.NewString()
	if rawNonce := strings.TrimSpace(os.Getenv("SYNARA_TEST_PLATFORM_ROUTING_NONCE")); rawNonce != "" {
		parsed, parseErr := uuid.Parse(rawNonce)
		if parseErr != nil || parsed == uuid.Nil || parsed.String() != rawNonce {
			t.Fatal("SYNARA_TEST_PLATFORM_ROUTING_NONCE must be a canonical lowercase UUID")
		}
		nonce = rawNonce
	}
	publication, err := SignPlatformAuthorityPublication(privateKey, PlatformAuthorityPublication{
		SchemaVersion:     PlatformAuthoritySchemaVersionV1,
		PublisherIdentity: "orbstack-routing-acceptance", KeyID: "rfc8032-test-key-v1",
		Nonce: nonce, IssuedAt: now.Format(time.RFC3339Nano),
		ExpiresAt:         now.Add(2 * time.Minute).Format(time.RFC3339Nano),
		ExecutionTargetID: targetID.String(), ObservedAt: now.Format(time.RFC3339Nano),
		Health: &PlatformAuthorityHealth{
			Status: HealthHealthy, CapacityStatus: CapacityAvailable,
			AvailableCapacityUnits: intPointer(20), AllocatedCapacityUnits: 5, TTLSeconds: 300,
		},
		DRReadiness: []PlatformAuthorityDRReadiness{{
			SourceDRDomain: "orbstack/source", DRDomain: "kind/destination",
			ReplicatedThroughAt: now.Add(-time.Second).Format(time.RFC3339Nano),
			ArtifactsReady:      true, CheckpointsReady: true, MemoryReady: true, TTLSeconds: 300,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(publication)
	if err != nil {
		t.Fatal(err)
	}
	endpoint := fmt.Sprintf(
		"%s/v1/platform/routing-authority/execution-targets/%s/observations",
		baseURL,
		targetID,
	)
	client := &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			DisableKeepAlives: true,
		},
	}

	type outcome struct {
		result PlatformAuthorityPublicationResult
		status int
		body   string
		err    error
	}
	const requestCount = 8
	start := make(chan struct{})
	outcomes := make(chan outcome, requestCount)
	var wait sync.WaitGroup
	for index := 0; index < requestCount; index++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			request, requestErr := http.NewRequest(http.MethodPut, endpoint, bytes.NewReader(body))
			if requestErr != nil {
				outcomes <- outcome{err: requestErr}
				return
			}
			request.Header.Set("Content-Type", "application/json")
			response, requestErr := client.Do(request)
			if requestErr != nil {
				outcomes <- outcome{err: requestErr}
				return
			}
			defer response.Body.Close()
			responseBody, readErr := io.ReadAll(response.Body)
			if readErr != nil {
				outcomes <- outcome{status: response.StatusCode, err: readErr}
				return
			}
			var result PlatformAuthorityPublicationResult
			decodeErr := json.Unmarshal(responseBody, &result)
			outcomes <- outcome{result: result, status: response.StatusCode, body: string(responseBody), err: decodeErr}
		}()
	}
	close(start)
	wait.Wait()
	close(outcomes)
	accepted := 0
	replayed := 0
	for item := range outcomes {
		if item.err != nil {
			t.Fatal(item.err)
		}
		if item.status != http.StatusOK {
			t.Fatalf("publication status=%d body=%s", item.status, item.body)
		}
		if item.result.ExecutionTargetID != targetID || item.result.Health == nil ||
			len(item.result.DRReadiness) != 1 {
			t.Fatalf("publication result=%#v", item.result)
		}
		if item.result.Replayed {
			replayed++
		} else {
			accepted++
		}
	}
	expectedAccepted := 1
	if strings.EqualFold(strings.TrimSpace(os.Getenv("SYNARA_TEST_PLATFORM_ROUTING_EXPECT_REPLAY_ONLY")), "true") ||
		strings.TrimSpace(os.Getenv("SYNARA_TEST_PLATFORM_ROUTING_EXPECT_REPLAY_ONLY")) == "1" {
		expectedAccepted = 0
	}
	if accepted != expectedAccepted || replayed != requestCount-expectedAccepted {
		t.Fatalf("accepted=%d replayed=%d", accepted, replayed)
	}
}
