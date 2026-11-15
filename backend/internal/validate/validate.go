// Package validate memvalidasi input pengguna, termasuk proteksi dasar SSRF
// untuk URL yang akan diproses oleh yt-dlp.
package validate

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"strings"
	"time"
)

// URL memvalidasi URL pengguna: skema http/https dan host tidak boleh
// mengarah ke IP privat/loopback/link-local (mencegah SSRF ke jaringan internal).
func URL(raw string) (*url.URL, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, fmt.Errorf("URL kosong")
	}
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("URL tidak valid")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("skema URL harus http/https")
	}
	host := u.Hostname()
	if host == "" {
		return nil, fmt.Errorf("URL tidak valid")
	}

	if ip := net.ParseIP(host); ip != nil {
		if isBlocked(ip) {
			return nil, fmt.Errorf("alamat tidak diizinkan")
		}
	} else {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
		if err != nil || len(ips) == 0 {
			return nil, fmt.Errorf("host tidak ditemukan")
		}
		for _, a := range ips {
			if isBlocked(a.IP) {
				return nil, fmt.Errorf("alamat tidak diizinkan")
			}
		}
	}
	return u, nil
}

func isBlocked(ip net.IP) bool {
	return ip.IsLoopback() ||
		ip.IsPrivate() ||
		ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() ||
		ip.IsUnspecified()
}
