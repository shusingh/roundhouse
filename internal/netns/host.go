package netns

import (
	"bufio"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"
)

// DefaultUplink returns the interface of the host's IPv4 default route, read
// from /proc/net/route (the default route's destination is 00000000).
func DefaultUplink() (string, error) {
	f, err := os.Open("/proc/net/route")
	if err != nil {
		return "", fmt.Errorf("read routes: %w", err)
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) > 2 && fields[1] == "00000000" && fields[0] != "Iface" {
			return fields[0], nil
		}
	}
	return "", errors.New("no IPv4 default route; set network.uplink")
}

// Namespaces lists named network namespaces with the Roundhouse prefix.
func Namespaces() ([]string, error) {
	entries, err := os.ReadDir("/var/run/netns")
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("list network namespaces: %w", err)
	}
	var out []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), NamePrefix) {
			out = append(out, e.Name())
		}
	}
	return out, nil
}
