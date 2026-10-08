package admission

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

type PriorityAssigner interface {
	AssignPriority(context.Context, string, string) (string, error)
}

type PriorityAssignerFunc func(context.Context, string, string) (string, error)

func (function PriorityAssignerFunc) AssignPriority(ctx context.Context, principalID string, intent string) (string, error) {
	return function(ctx, principalID, intent)
}

// TrustedPriorityPolicy treats client input as an intent, never as a priority grant. A principal
// receives only the configured classes; missing/empty intent falls back to the least privileged
// configured default.
type TrustedPriorityPolicy struct {
	defaultClass string
	grants       map[string]map[string]struct{}
}

func NewTrustedPriorityPolicy(defaultClass string, grants map[string][]string) (*TrustedPriorityPolicy, error) {
	if !validPriorityClass(defaultClass) {
		return nil, errors.New("trusted priority default is invalid")
	}
	policy := &TrustedPriorityPolicy{defaultClass: defaultClass, grants: make(map[string]map[string]struct{}, len(grants))}
	for principalID, classes := range grants {
		principalID = strings.TrimSpace(principalID)
		if principalID == "" {
			return nil, errors.New("trusted priority principal is empty")
		}
		allowed := map[string]struct{}{defaultClass: {}}
		for _, class := range classes {
			if !validPriorityClass(class) {
				return nil, fmt.Errorf("trusted priority class %q is invalid", class)
			}
			allowed[class] = struct{}{}
		}
		policy.grants[principalID] = allowed
	}
	return policy, nil
}

func (policy *TrustedPriorityPolicy) AssignPriority(_ context.Context, principalID string, intent string) (string, error) {
	intent = strings.TrimSpace(intent)
	if intent == "" {
		return policy.defaultClass, nil
	}
	if !validPriorityClass(intent) {
		return "", fmt.Errorf("unsupported priority intent %q", intent)
	}
	allowed := policy.grants[strings.TrimSpace(principalID)]
	if _, ok := allowed[intent]; !ok {
		return "", fmt.Errorf("principal is not authorized for priority intent %q", intent)
	}
	return intent, nil
}

func validPriorityClass(class string) bool {
	return class == "publish" || class == "preview" || class == "rebuild" || class == "migration"
}
