package adobe

import (
	"errors"
	"strings"
)

const (
	refreshURL = "https://adobeid-na1.services.adobe.com/ims/check/v6/token?jslVersion=v2-v0.54.0-3-g58cfcb7"
	clientID   = "clio-playground-web"
	scopeValue = "AdobeID,firefly_api,openid,pps.read,pps.write,additional_info.projectedProductContext,additional_info.ownerOrg,uds_read,uds_write,ab.manage,read_organizations,additional_info.roles,account_cluster.read,creative_production,tk_platform,tk_platform_sync,profile"
)

var ErrAdobeCookieEmpty = errors.New("cookie is empty")

type CookieExchangeResult struct {
	AccessToken string
	ExpiresIn   int
	Raw         map[string]any
}

func normalizeCookie(v string) string {
	v = strings.TrimSpace(v)
	if strings.HasPrefix(strings.ToLower(v), "cookie:") {
		v = strings.TrimSpace(v[len("cookie:"):])
	}
	return v
}
