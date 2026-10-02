// Package audit probes passive Linux Audit access without modifying audit rules.
package audit

import (
	"github.com/elastic/go-libaudit/v2"
)

// Status reports transport access separately from implemented attribution.
type Status struct {
	Reason    string `json:"reason"`
	Available bool   `json:"available"`
	Usable    bool   `json:"usable"`
}

// Probe opens only a multicast subscriber: no AUDIT_SET, controller claim or rules.
func Probe() Status {
	client, err := libaudit.NewMulticastAuditClient(nil)
	if err != nil {
		return Status{Reason: "passive Audit multicast unavailable: " + err.Error()}
	}

	err = client.Close()
	if err != nil {
		return Status{Reason: err.Error()}
	}

	return Status{
		Available: true,
		Usable:    true,
		Reason:    "passive attribution available; requires administrator rules with the configured key",
	}
}
