package executiontargets

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/synara-ai/synara/services/control-plane/internal/placement"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
)

const (
	kubernetesWorkerDefaultPriorityClassName = "synara-worker-nonpreempting-v1"
	kubernetesWorkerPodSpecRevision          = "v3-operator-observability"
)

type kubernetesPriorityClass struct {
	Name             string
	Value            int32
	GlobalDefault    bool
	PreemptionPolicy string
}

func validateKubernetesWorkerPodPriorityClass(
	ctx context.Context,
	client kubernetesClient,
	pod map[string]any,
	cache map[string]error,
) error {
	spec, ok := pod["spec"].(map[string]any)
	if !ok {
		return problem.New(500, "kubernetes_worker_pod_priority_invalid", "Kubernetes Worker Pod priority policy is missing.")
	}
	className, ok := spec["priorityClassName"].(string)
	className = strings.TrimSpace(className)
	if !ok || className == "" || spec["preemptionPolicy"] != placement.KubernetesPreemptionPolicyNever {
		return problem.New(500, "kubernetes_worker_pod_priority_invalid", "Kubernetes Worker Pod priority policy is missing.")
	}
	if cached, found := cache[className]; found {
		return cached
	}
	priorityClass, err := client.GetPriorityClass(ctx, className)
	if err != nil {
		var statusErr *kubernetesAPIStatusError
		if errors.As(err, &statusErr) && statusErr.StatusCode == http.StatusNotFound {
			err = problem.Wrap(
				409,
				"worker_pool_priority_class_not_found",
				"Kubernetes Worker pool priorityClassName does not exist in the target cluster.",
				err,
			)
		} else {
			err = problem.Wrap(
				502,
				"kubernetes_priority_class_read_failed",
				"Kubernetes Worker PriorityClass could not be read from the target cluster.",
				err,
			)
		}
		cache[className] = err
		return err
	}
	if strings.TrimSpace(priorityClass.Name) != className ||
		strings.TrimSpace(priorityClass.PreemptionPolicy) != placement.KubernetesPreemptionPolicyNever {
		err = problem.New(
			409,
			"worker_pool_preemption_unsupported",
			"Kubernetes Worker PriorityClass must declare preemptionPolicy Never.",
		)
		cache[className] = err
		return err
	}
	if className == kubernetesWorkerDefaultPriorityClassName &&
		(priorityClass.Value != 0 || priorityClass.GlobalDefault) {
		err = problem.New(
			409,
			"kubernetes_default_priority_class_invalid",
			"The default Kubernetes Worker PriorityClass must have value 0 and globalDefault false.",
		)
		cache[className] = err
		return err
	}
	cache[className] = nil
	return nil
}
