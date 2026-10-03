package config

import "strings"

// supportedDrivers lists the adapters VeriDB can talk to. Adding a driver means
// implementing database.Driver and registering its name here.
//
// The list lives in the config package on purpose: validation needs it, and the
// database package imports config (never the other way around).
var supportedDrivers = []string{"postgres"}

// SupportedDrivers returns the driver names VeriDB understands.
func SupportedDrivers() []string {
	out := make([]string, len(supportedDrivers))
	copy(out, supportedDrivers)
	return out
}

// IsSupportedDriver reports whether d is a known driver name.
func IsSupportedDriver(d string) bool {
	d = strings.ToLower(strings.TrimSpace(d))
	for _, s := range supportedDrivers {
		if s == d {
			return true
		}
	}
	return false
}

func isSupportedAuditSink(name string) bool {
	return containsFold(SupportedAuditSinks(), strings.TrimSpace(name))
}

func containsFold(haystack []string, needle string) bool {
	for _, item := range haystack {
		if strings.EqualFold(item, needle) {
			return true
		}
	}
	return false
}
