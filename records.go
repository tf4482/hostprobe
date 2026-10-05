package main

import (
	"fmt"
	"net/netip"
	"strings"
)

// Validated host ready for checking
type HostRecord struct {
	ID        any
	Name      string
	Method    string
	IP        string
	IPVersion int
	SSHUser   string
	SSHPort   int
}

// Normalize one row; returns a skip reason when unusable
func validateRecord(raw RawRecord) (HostRecord, string) {
	record := HostRecord{ID: raw.ID, Name: displayName(raw.Name, raw.ID)}
	if name, ok := raw.Name.(string); !ok || strings.TrimSpace(name) == "" {
		return record, "name is missing"
	}
	method, ok := raw.Method.(string)
	if !ok {
		return record, "method is missing"
	}
	record.Method = strings.ToLower(strings.TrimSpace(method))
	if record.Method != "ping" && record.Method != "ssh" {
		if record.Method == "" {
			return record, "unknown method: <empty>"
		}
		return record, "unknown method: " + record.Method
	}

	address, reason := parseIP(raw.IP)
	if reason != "" {
		return record, reason
	}
	record.IP = address.String()
	record.IPVersion = 6
	if address.Is4() {
		record.IPVersion = 4
	}
	if record.Method == "ping" {
		return record, ""
	}

	user, ok := raw.SSHUser.(string)
	record.SSHUser = strings.TrimSpace(user)
	if !ok || record.SSHUser == "" {
		return record, "ssh_user is missing"
	}
	port, ok := integer(raw.SSHPort)
	if !ok || port < 1 || port > 65535 {
		return record, "ssh_port must be an integer from 1 to 65535"
	}
	record.SSHPort = int(port)
	return record, ""
}

// Accept text and native inet values
func parseIP(value any) (netip.Addr, string) {
	switch ip := value.(type) {
	case string:
		if strings.TrimSpace(ip) == "" {
			break
		}
		address, err := netip.ParseAddr(strings.TrimSpace(ip))
		if err != nil {
			return netip.Addr{}, "target is not a valid IPv4 or IPv6 address"
		}
		return address, ""
	case netip.Addr:
		if ip.IsValid() {
			return ip, ""
		}
	case netip.Prefix:
		if ip.IsValid() && ip.IsSingleIP() {
			return ip.Addr(), ""
		}
		return netip.Addr{}, "target is not a valid IPv4 or IPv6 address"
	}
	return netip.Addr{}, "target IP address is missing"
}

// Integer column value of any width
func integer(value any) (int64, bool) {
	switch number := value.(type) {
	case int16:
		return int64(number), true
	case int32:
		return int64(number), true
	case int64:
		return number, true
	}
	return 0, false
}

// Row label for output, falling back to the identifier
func displayName(name, id any) string {
	if text, ok := name.(string); ok && strings.TrimSpace(text) != "" {
		return strings.TrimSpace(text)
	}
	switch value := id.(type) {
	case string:
		return fmt.Sprintf("row %q", value)
	case [16]byte:
		return fmt.Sprintf("row %x-%x-%x-%x-%x", value[0:4], value[4:6], value[6:8], value[8:10], value[10:16])
	}
	return fmt.Sprintf("row %v", id)
}
