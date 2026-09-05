package callback

import (
	"fmt"
	"net/url"
	"strings"

	"github.com/enerplanet/tentacron/internal/config"
)

// Check decides whether a callback_url may be accepted: https, no user
// info, and a host that matches callbacks.allowed_hosts exactly (host:port
// when the URL carries a port). Request data never supplies an outbound
// URL anywhere else, and this is the deliberate, fenced exception.
func Check(cfg config.Callbacks, raw string) error {
	if !cfg.Enabled() {
		return ErrDisabled
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("callback_url is not a valid URL")
	}
	if u.Scheme != "https" {
		return fmt.Errorf("callback_url must use https")
	}
	if u.User != nil {
		return fmt.Errorf("callback_url must not carry credentials")
	}
	if u.Host == "" {
		return fmt.Errorf("callback_url has no host")
	}
	for _, h := range cfg.AllowedHosts {
		if strings.EqualFold(h, u.Host) {
			return nil
		}
	}
	return fmt.Errorf("callback_url host %q is not allow-listed", u.Host)
}
