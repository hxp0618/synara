package executiontargets

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
)

type kubernetesHTTPFactory struct{}

func (kubernetesHTTPFactory) Open(configuration kubernetesTargetConfiguration) (kubernetesClient, error) {
	return newKubernetesHTTPClient(configuration)
}

type kubernetesHTTPClient struct {
	baseURL string
	token   string
	client  *http.Client
}

func (c *kubernetesHTTPClient) Apply(ctx context.Context, path string, object map[string]any) error {
	query := url.Values{"fieldManager": {"synara-control-plane"}, "force": {"true"}}
	return c.do(ctx, http.MethodPatch, path+"?"+query.Encode(), object, nil, http.StatusOK, http.StatusCreated)
}

func (c *kubernetesHTTPClient) ListPods(ctx context.Context, namespace string, targetID uuid.UUID) ([]kubernetesPod, error) {
	return c.listPods(ctx, namespace, kubernetesTargetLabel+"="+targetID.String())
}

func (c *kubernetesHTTPClient) ListPodUIDs(ctx context.Context, namespace string) ([]string, error) {
	pods, err := c.listPods(ctx, namespace, "")
	if err != nil {
		return nil, err
	}
	uids := make([]string, 0, len(pods))
	for _, pod := range pods {
		uids = append(uids, pod.UID)
	}
	return uids, nil
}

func (c *kubernetesHTTPClient) listPods(ctx context.Context, namespace, labelSelector string) ([]kubernetesPod, error) {
	items := make([]kubernetesPod, 0)
	continueToken := ""
	seenContinueTokens := map[string]struct{}{}
	for {
		var response struct {
			Metadata struct {
				Continue string `json:"continue"`
			} `json:"metadata"`
			Items []struct {
				Metadata struct {
					Name              string            `json:"name"`
					UID               string            `json:"uid"`
					CreationTimestamp time.Time         `json:"creationTimestamp"`
					Labels            map[string]string `json:"labels"`
					Annotations       map[string]string `json:"annotations"`
				} `json:"metadata"`
				Status struct {
					Phase      string `json:"phase"`
					Reason     string `json:"reason"`
					Conditions []struct {
						Type   string `json:"type"`
						Status string `json:"status"`
						Reason string `json:"reason"`
					} `json:"conditions"`
					ContainerStatuses []struct {
						Name  string `json:"name"`
						State struct {
							Waiting *struct {
								Reason string `json:"reason"`
							} `json:"waiting"`
							Terminated *struct {
								ExitCode int    `json:"exitCode"`
								Reason   string `json:"reason"`
							} `json:"terminated"`
						} `json:"state"`
						LastState struct {
							Terminated *struct {
								Reason string `json:"reason"`
							} `json:"terminated"`
						} `json:"lastState"`
					} `json:"containerStatuses"`
				} `json:"status"`
			} `json:"items"`
		}
		query := url.Values{}
		query.Set("limit", "100")
		if labelSelector != "" {
			query.Set("labelSelector", labelSelector)
		}
		if continueToken != "" {
			query.Set("continue", continueToken)
		}
		path := "/api/v1/namespaces/" + url.PathEscape(namespace) + "/pods"
		if encoded := query.Encode(); encoded != "" {
			path += "?" + encoded
		}
		if err := c.do(ctx, http.MethodGet, path, nil, &response, http.StatusOK); err != nil {
			return nil, err
		}
		for _, item := range response.Items {
			conditions := make([]kubernetesPodCondition, 0, len(item.Status.Conditions))
			for _, condition := range item.Status.Conditions {
				conditions = append(conditions, kubernetesPodCondition{
					Type: condition.Type, Status: condition.Status, Reason: condition.Reason,
				})
			}
			containers := make([]kubernetesContainerStatus, 0, len(item.Status.ContainerStatuses))
			for _, status := range item.Status.ContainerStatuses {
				container := kubernetesContainerStatus{Name: status.Name}
				if status.State.Waiting != nil {
					container.WaitingReason = status.State.Waiting.Reason
				}
				if status.State.Terminated != nil {
					container.Terminated = true
					container.ExitCode = status.State.Terminated.ExitCode
					container.TerminatedReason = status.State.Terminated.Reason
				}
				if status.LastState.Terminated != nil {
					container.LastTerminatedReason = status.LastState.Terminated.Reason
				}
				containers = append(containers, container)
			}
			items = append(items, kubernetesPod{
				Name: item.Metadata.Name, UID: item.Metadata.UID, Phase: item.Status.Phase,
				Reason: item.Status.Reason, CreatedAt: item.Metadata.CreationTimestamp.UTC(),
				Labels: item.Metadata.Labels, Annotations: item.Metadata.Annotations,
				Conditions: conditions, Containers: containers,
			})
		}
		continueToken = strings.TrimSpace(response.Metadata.Continue)
		if continueToken == "" {
			break
		}
		if _, seen := seenContinueTokens[continueToken]; seen {
			return nil, errors.New("Kubernetes Pod list repeated its continuation token")
		}
		seenContinueTokens[continueToken] = struct{}{}
	}
	return items, nil
}

func (c *kubernetesHTTPClient) DeletePod(ctx context.Context, namespace, name, uid string) error {
	uid = strings.TrimSpace(uid)
	if uid == "" {
		return errors.New("Kubernetes Pod UID is required for safe deletion")
	}
	path := kubernetesNamespacedPath(namespace, "pods", name) + "?gracePeriodSeconds=30&propagationPolicy=Background"
	err := c.do(ctx, http.MethodDelete, path, map[string]any{
		"apiVersion": "v1", "kind": "DeleteOptions",
		"preconditions": map[string]any{"uid": uid},
	}, nil, http.StatusOK, http.StatusAccepted, http.StatusNotFound)
	var statusErr *kubernetesAPIStatusError
	if errors.As(err, &statusErr) && statusErr.StatusCode == http.StatusConflict {
		return fmt.Errorf("%w: %s", errKubernetesPodUIDPreconditionFailed, statusErr.Detail)
	}
	return err
}

func (c *kubernetesHTTPClient) do(
	ctx context.Context,
	method, path string,
	input, output any,
	accepted ...int,
) error {
	var body io.Reader
	if input != nil {
		encoded, err := json.Marshal(input)
		if err != nil {
			return err
		}
		body = bytes.NewReader(encoded)
	}
	request, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, body)
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+c.token)
	if input != nil {
		contentType := "application/json"
		if method == http.MethodPatch {
			contentType = "application/apply-patch+yaml"
		}
		request.Header.Set("Content-Type", contentType)
	}
	response, err := c.client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	acceptedStatus := false
	for _, status := range accepted {
		if response.StatusCode == status {
			acceptedStatus = true
			break
		}
	}
	if !acceptedStatus {
		var status struct {
			Reason  string `json:"reason"`
			Message string `json:"message"`
		}
		_ = json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&status)
		detail := strings.TrimSpace(status.Reason)
		if message := strings.TrimSpace(status.Message); message != "" {
			if detail != "" {
				detail += ": "
			}
			detail += message
		}
		if detail == "" {
			detail = http.StatusText(response.StatusCode)
		}
		return &kubernetesAPIStatusError{StatusCode: response.StatusCode, Detail: detail}
	}
	if output == nil {
		_, _ = io.Copy(io.Discard, response.Body)
		return nil
	}
	return json.NewDecoder(io.LimitReader(response.Body, 4<<20)).Decode(output)
}
