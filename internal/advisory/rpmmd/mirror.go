package rpmmd

import (
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// ResolveMirror gives a base URL for mirrorlist=URL or metalink=URL: the
// first http(s) line of a mirror list, or the first URL of a metalink whose
// path ends in repodata/repomd.xml, without that suffix.
func ResolveMirror(ctx context.Context, client *http.Client, spec string) (string, error) {
	kind, u, _ := strings.Cut(spec, "=")
	if client == nil {
		client = http.DefaultClient
	}
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return "", err
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%s: %s", u, resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return "", err
	}
	if kind == "mirrorlist" {
		for _, l := range strings.Split(string(body), "\n") {
			l = strings.TrimSpace(l)
			if strings.HasPrefix(l, "http://") || strings.HasPrefix(l, "https://") {
				return l, nil
			}
		}
		return "", fmt.Errorf("%s: the mirror list has no URL", u)
	}
	var ml struct {
		URLs []string `xml:"files>file>resources>url"`
	}
	if err := xml.Unmarshal(body, &ml); err != nil {
		return "", fmt.Errorf("%s: metalink: %w", u, err)
	}
	for _, x := range ml.URLs {
		x = strings.TrimSpace(x)
		if strings.HasPrefix(x, "http") && strings.HasSuffix(x, "repodata/repomd.xml") {
			return strings.TrimSuffix(x, "repodata/repomd.xml"), nil
		}
	}
	return "", fmt.Errorf("%s: the metalink has no repomd.xml URL", u)
}
