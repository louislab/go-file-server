package netstate

import (
	"fmt"
	"net"
	"os"
	"sort"
	"strings"
	"time"
	"unicode"
)

type Address struct {
	Interface string `json:"interface"`
	IP        string `json:"ip"`
	URL       string `json:"url"`
	Kind      string `json:"kind"`
}

type HostInfo struct {
	Hostname    string    `json:"hostname"`
	BonjourName string    `json:"bonjourName,omitempty"`
	BonjourURL  string    `json:"bonjourURL,omitempty"`
	UploadURL   string    `json:"uploadURL"`
	Addresses   []Address `json:"addresses"`
	Warnings    []string  `json:"warnings,omitempty"`
	UpdatedAt   time.Time `json:"updatedAt"`
}

func SnapshotHTTP(listenAddr string) HostInfo {
	host, port := SplitListenAddr(listenAddr)
	hostname, _ := os.Hostname()
	hostname = strings.TrimSpace(hostname)
	bonjourHost := bonjourHostname(hostname)

	info := HostInfo{
		Hostname:  hostname,
		Addresses: addressesForHost(host, port),
		UpdatedAt: time.Now().UTC(),
	}

	if bonjourHost != "" {
		info.BonjourName = bonjourHost
		info.BonjourURL = fmt.Sprintf("http://%s:%s", bonjourHost, port)
	}

	if host != "" && !isWildcardHost(host) {
		info.UploadURL = fmt.Sprintf("http://%s:%s", host, port)
		if len(info.Addresses) == 0 {
			info.Addresses = []Address{{
				Interface: "configured",
				IP:        host,
				URL:       info.UploadURL,
				Kind:      hostKind(host),
			}}
		}
		return info
	}

	if len(info.Addresses) > 0 {
		info.UploadURL = info.Addresses[0].URL
	} else if info.BonjourURL != "" {
		info.UploadURL = info.BonjourURL
		info.Warnings = append(info.Warnings, "No active private or link-local IPv4 address detected on the host.")
	} else {
		info.UploadURL = fmt.Sprintf("http://localhost:%s", port)
		info.Warnings = append(info.Warnings, "No active external network interface detected; only localhost is currently available.")
	}

	return info
}

func DisplayPublicURL(listenAddr string) string {
	return SnapshotHTTP(listenAddr).UploadURL
}

func SplitListenAddr(addr string) (string, string) {
	host, port, err := net.SplitHostPort(addr)
	if err == nil {
		return host, port
	}

	trimmed := strings.TrimSpace(addr)
	if strings.HasPrefix(trimmed, ":") {
		return "", strings.TrimPrefix(trimmed, ":")
	}

	lastColon := strings.LastIndex(trimmed, ":")
	if lastColon > 0 && lastColon < len(trimmed)-1 {
		return trimmed[:lastColon], trimmed[lastColon+1:]
	}

	return trimmed, "80"
}

func addressesForHost(host string, port string) []Address {
	interfaces, err := net.Interfaces()
	if err != nil {
		return []Address{}
	}

	addresses := make([]Address, 0)
	seen := make(map[string]struct{})

	for _, iface := range interfaces {
		if !isPeerReachableInterface(iface) {
			continue
		}

		ifaceAddrs, err := iface.Addrs()
		if err != nil {
			continue
		}

		for _, addr := range ifaceAddrs {
			ipNet, ok := addr.(*net.IPNet)
			if !ok {
				continue
			}

			ip := ipNet.IP.To4()
			if ip == nil {
				continue
			}

			kind := ipKind(ip)
			if kind == "" {
				continue
			}

			ipText := ip.String()
			if _, ok := seen[ipText]; ok {
				continue
			}
			seen[ipText] = struct{}{}

			addresses = append(addresses, Address{
				Interface: iface.Name,
				IP:        ipText,
				URL:       fmt.Sprintf("http://%s:%s", ipText, port),
				Kind:      kind,
			})
		}
	}

	sort.Slice(addresses, func(i int, j int) bool {
		left := addressRank(addresses[i].Kind)
		right := addressRank(addresses[j].Kind)
		if left != right {
			return left < right
		}
		leftInterfaceRank := interfaceRank(addresses[i].Interface)
		rightInterfaceRank := interfaceRank(addresses[j].Interface)
		if leftInterfaceRank != rightInterfaceRank {
			return leftInterfaceRank < rightInterfaceRank
		}
		if addresses[i].Interface != addresses[j].Interface {
			return addresses[i].Interface < addresses[j].Interface
		}
		return addresses[i].IP < addresses[j].IP
	})

	return addresses
}

func ipKind(ip net.IP) string {
	switch {
	case ip.IsPrivate():
		return "private"
	case ip.IsLinkLocalUnicast():
		return "link-local"
	case ip.IsGlobalUnicast():
		return "public"
	default:
		return ""
	}
}

func addressRank(kind string) int {
	switch kind {
	case "private":
		return 0
	case "link-local":
		return 1
	case "public":
		return 2
	default:
		return 3
	}
}

func isPeerReachableInterface(iface net.Interface) bool {
	if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
		return false
	}
	if iface.Flags&net.FlagPointToPoint != 0 {
		return false
	}

	name := strings.ToLower(strings.TrimSpace(iface.Name))
	for _, prefix := range []string{"utun", "awdl", "llw", "gif", "stf", "tun", "tap", "anpi", "docker", "veth", "vmnet", "tailscale", "wg", "ipsec"} {
		if strings.HasPrefix(name, prefix) {
			return false
		}
	}

	return true
}

func interfaceRank(name string) int {
	trimmed := strings.ToLower(strings.TrimSpace(name))
	switch {
	case strings.HasPrefix(trimmed, "en"):
		return 0
	case strings.HasPrefix(trimmed, "eth"):
		return 1
	case strings.HasPrefix(trimmed, "bridge"):
		return 2
	case strings.HasPrefix(trimmed, "wlan"), strings.HasPrefix(trimmed, "wifi"):
		return 3
	default:
		return 4
	}
}

func bonjourHostname(hostname string) string {
	cleaned := strings.TrimSpace(strings.ToLower(hostname))
	if cleaned == "" {
		return ""
	}

	var builder strings.Builder
	lastWasDash := false
	for _, char := range cleaned {
		switch {
		case unicode.IsLetter(char), unicode.IsDigit(char):
			builder.WriteRune(char)
			lastWasDash = false
		case char == '-', char == '.':
			if builder.Len() == 0 || lastWasDash {
				continue
			}
			builder.WriteByte('-')
			lastWasDash = true
		case unicode.IsSpace(char), char == '_':
			if builder.Len() == 0 || lastWasDash {
				continue
			}
			builder.WriteByte('-')
			lastWasDash = true
		}
	}

	value := strings.Trim(builder.String(), "-")
	if value == "" {
		return ""
	}
	return value + ".local"
}

func isWildcardHost(host string) bool {
	trimmed := strings.TrimSpace(strings.ToLower(host))
	return trimmed == "" || trimmed == "0.0.0.0" || trimmed == "::"
}

func hostKind(host string) string {
	ip := net.ParseIP(host)
	if ip == nil {
		return "configured"
	}
	if ipv4 := ip.To4(); ipv4 != nil {
		return ipKind(ipv4)
	}
	return "configured"
}
