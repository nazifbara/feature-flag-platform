package evaluator

import (
	"errors"
	"fmt"
	"hash/fnv"
	"uuid"
)

const (
	MasterSwitchReason   = "master_switch"
	AllowlistMatchReason = "allowlist_match"
	RuleMatchReason      = "rule_match="
	FallthroughReason    = "fallthrough"
)

func Evaluate(flagConfig FlagConfig, attributes map[string][]any) (on bool, reason string, err error) {
	if !flagConfig.enabled {
		return false, MasterSwitchReason, nil
	}
	if flagConfig.salt == "" {
		return false, "", errors.New("flag config with no salt")
	}
	if flagConfig.id == uuid.Nil() {
		return false, "", errors.New("flag config with no id")
	}
	userKey, ok := attributes["user_key"]
	userKeyString := ""
	if ok {
		if len(userKey) != 1 {
			return false, "", fmt.Errorf("the user_key attribute must have exactly one value, got: %d", len(userKey))
		}
		userKeyString, ok = userKey[0].(string)
		if !ok {
			return false, "", errors.New("the user_key attribute must be a string")
		}
		if userKeyString == "" {
			return false, "", fmt.Errorf("the user_key attribute value is required")
		}
		if _, ok = flagConfig.allowlist[userKeyString]; ok {
			return true, AllowlistMatchReason, nil
		}
	} else {
		return false, "", errors.New("the user_key attribute is required")
	}

	userBucket := bucket(flagConfig.salt, flagConfig.id, userKeyString)

	for i, rule := range flagConfig.rules {
		serve, matched, err := rule.Match(attributes)
		if err != nil {
			return false, "", fmt.Errorf("rule at index %d errors with: %w", i, err)
		}
		if matched {
			return userBucket < serve, RuleMatchReason + rule.id.String(), nil
		}
	}

	return userBucket < flagConfig.fall, FallthroughReason, nil
}

// bucket gives the user's rollout percentage bucket in 0.01% granularity.
// It hashes the flag's ID, not its key, so renaming a flag doesn't reshuffle
// its users.
func bucket(salt string, flagID uuid.UUID, userKey string) float32 {
	h := fnv.New32a()
	// hash.Hash's Write never returns an error.
	_, _ = h.Write([]byte(salt + "." + flagID.String() + "." + userKey))
	return float32(h.Sum32()%10000) / 10000
}
