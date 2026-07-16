package newapi

import (
	"regexp"
	"strconv"
	"strings"
)

// Only exact stable releases and exact -rc.N prereleases are understood.
// Any other suffix is an unknown contract and must remain read-only.
var versionPattern = regexp.MustCompile(`(?i)^v?(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(?:-rc\.(0|[1-9][0-9]*))?$`)

type Capabilities struct {
	Version                    string `json:"version"`
	Known                      bool   `json:"known"`
	AdminUserManage            bool   `json:"admin_user_manage"`
	RedemptionAPI              bool   `json:"redemption_api"`
	UpstreamRequestID          bool   `json:"upstream_request_id"`
	SubscriptionBilling        bool   `json:"subscription_billing"`
	ClickHouseLogs             bool   `json:"clickhouse_logs"`
	HardDeleteSafe             bool   `json:"hard_delete_safe"`
	UnknownVersionReadOnly     bool   `json:"unknown_version_read_only"`
	MinimumKnownRelease        string `json:"minimum_known_release"`
	HardDeleteMinimumKnownSafe string `json:"hard_delete_minimum_known_safe"`
}

type parsedVersion struct {
	major, minor, patch int
	rc                  int
	hasRC               bool
}

func DetectCapabilities(version string) Capabilities {
	result := Capabilities{
		Version:                    strings.TrimSpace(version),
		UnknownVersionReadOnly:     true,
		MinimumKnownRelease:        "v1.0.0-rc.21",
		HardDeleteMinimumKnownSafe: "v1.0.0-rc.22 or a stable v1.0.0+ release",
	}
	parsed, ok := parseVersion(version)
	if !ok || parsed.major < 1 {
		return result
	}

	// The adapter contract is verified against the v1.0 release line. Unknown
	// future majors remain read-only until contract tests explicitly cover them.
	if parsed.major != 1 || parsed.minor != 0 || parsed.patch != 0 {
		return result
	}
	if parsed.hasRC && parsed.rc < 21 {
		return result
	}

	result.Known = true
	result.UnknownVersionReadOnly = false
	result.AdminUserManage = true
	result.RedemptionAPI = true
	result.UpstreamRequestID = true
	result.SubscriptionBilling = true
	result.ClickHouseLogs = true
	// NewAPI PR #6168 fixed incomplete hard-delete cleanup after rc.21. The
	// first presumed safe release is rc.22; stable v1.0.0 is also treated safe.
	result.HardDeleteSafe = !parsed.hasRC || parsed.rc >= 22
	return result
}

func parseVersion(raw string) (parsedVersion, bool) {
	matches := versionPattern.FindStringSubmatch(strings.TrimSpace(raw))
	if len(matches) == 0 {
		return parsedVersion{}, false
	}
	values := make([]int, 3)
	for i := range values {
		value, err := strconv.Atoi(matches[i+1])
		if err != nil {
			return parsedVersion{}, false
		}
		values[i] = value
	}
	result := parsedVersion{major: values[0], minor: values[1], patch: values[2]}
	if matches[4] != "" {
		rc, err := strconv.Atoi(matches[4])
		if err != nil {
			return parsedVersion{}, false
		}
		result.rc = rc
		result.hasRC = true
	}
	return result, true
}
