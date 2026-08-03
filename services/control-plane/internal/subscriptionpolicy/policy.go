package subscriptionpolicy

import "errors"

const Version = "effective-subscription-v1"

var ErrUnknownStatus = errors.New("subscription status is not covered by effective-subscription-v1")

type Decision struct {
	EntitlementsActive bool
	Grace              bool
	Reason             string
}

func Evaluate(status string) (Decision, error) {
	switch status {
	case "trialing":
		return Decision{EntitlementsActive: true, Reason: "trialing"}, nil
	case "active":
		return Decision{EntitlementsActive: true, Reason: "active"}, nil
	case "suspended":
		return Decision{Reason: "internally_suspended"}, nil
	case "cancelled":
		return Decision{Reason: "internally_cancelled"}, nil
	default:
		return Decision{}, ErrUnknownStatus
	}
}
