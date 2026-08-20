package tailscaleapi

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// Grant is one tenant's slice of the ACL: who (Src, Tailscale logins) can
// reach which devices (Dst, their current Tailscale IPs) — IPs, not tags:
// Tailscale requires every tag an OAuth client can assign to be
// pre-declared when the credential is created, which would mean a manual
// Tailscale-console step every time a new tenant is created. IPs need no
// such pre-declaration, at the cost of needing a resync whenever a
// device's Tailscale IP changes (see docs/MULTI-TENANCY.md Tappa 3b).
type Grant struct {
	Src []string
	Dst []string
}

const (
	beginMarker  = "// farsight:begin"
	endMarker    = "// farsight:end"
	grantsAnchor = `"grants": [`
)

// UpsertGrantsBlock returns policy with Farsight's own grants — and only
// those — replaced or inserted. Everything else in the file (comments,
// any grants an admin wrote by hand, ssh rules, ...) passes through
// completely untouched: this is a surgical text edit, not a parse of the
// whole HuJSON document, on purpose (see package doc on GetACL).
func UpsertGrantsBlock(policy string, grants []Grant) (string, error) {
	var block strings.Builder
	block.WriteString(beginMarker + "\n")
	for _, g := range grants {
		srcJSON, err := json.Marshal(g.Src)
		if err != nil {
			return "", err
		}
		dstJSON, err := json.Marshal(g.Dst)
		if err != nil {
			return "", err
		}
		fmt.Fprintf(&block, "\t\t{\"src\": %s, \"dst\": %s, \"ip\": [\"*\"]},\n", srcJSON, dstJSON)
	}
	block.WriteString("\t\t" + endMarker)

	beginIdx := strings.Index(policy, beginMarker)
	endIdx := strings.Index(policy, endMarker)
	if beginIdx != -1 && endIdx != -1 && endIdx > beginIdx {
		return policy[:beginIdx] + block.String() + policy[endIdx+len(endMarker):], nil
	}

	idx := strings.Index(policy, grantsAnchor)
	if idx == -1 {
		return "", fmt.Errorf(`tailscaleapi: no %q array found in policy — is this tailnet using the older "acls" schema instead?`, "grants")
	}
	insertAt := idx + len(grantsAnchor)
	return policy[:insertAt] + "\n\t\t" + block.String() + "\n" + policy[insertAt:], nil
}

// openAccessGrant is what a fresh tailnet ships with by default — every
// new Tailscale ACL policy is created with this exact rule as its one
// "Example/default ACLs for unrestricted connections" example (found live
// on the real tailnet this was developed against — it was never a
// deliberate customization). It matters here because grants are OR'd: as
// long as this rule exists anywhere in the file, it alone makes every
// tenant-scoped grant Farsight writes redundant — anyone on the tailnet
// can already reach any device directly by IP, regardless of tenant. See
// docs/MULTI-TENANCY.md.
var (
	openSrcRe    = regexp.MustCompile(`"src"\s*:\s*\[\s*"\*"\s*\]`)
	openDstRe    = regexp.MustCompile(`"dst"\s*:\s*\[\s*"\*"\s*\]`)
	grantBlockRe = regexp.MustCompile(`\{[^{}]*\}`)
)

// openAccessReplacement is Tailscale's own recommended pattern for
// replacing the default "allow all" rule without losing legitimate
// access: every user can still always reach their own devices
// (autogroup:self), nobody gets free access to anyone else's — Farsight's
// own per-tenant grants (the farsight:begin/end block) still layer
// cross-user access on top of this, unaffected.
const openAccessReplacement = `{"src": ["autogroup:member"], "dst": ["autogroup:self"], "ip": ["*"]}`

// findOpenAccessGrant returns the byte span of the first grant object in
// policy — outside Farsight's own managed block, which never contains one
// of these by construction (see UpsertGrantsBlock) — that grants "*" to
// "*". ok is false if none is found (nothing to warn about or fix).
func findOpenAccessGrant(policy string) (start, end int, ok bool) {
	beginIdx := strings.Index(policy, beginMarker)
	endIdx := strings.Index(policy, endMarker)

	for _, m := range grantBlockRe.FindAllStringIndex(policy, -1) {
		s, e := m[0], m[1]
		if beginIdx != -1 && endIdx != -1 && s >= beginIdx && e <= endIdx+len(endMarker) {
			continue // inside Farsight's own block — never the target
		}
		block := policy[s:e]
		if openSrcRe.MatchString(block) && openDstRe.MatchString(block) {
			return s, e, true
		}
	}
	return 0, 0, false
}

// HasOpenAccessGrant reports whether policy still has the default
// "allow all" rule described above, outside Farsight's own managed block.
func HasOpenAccessGrant(policy string) bool {
	_, _, ok := findOpenAccessGrant(policy)
	return ok
}

// FixOpenAccessGrant replaces the first open/wildcard grant found (see
// HasOpenAccessGrant) with openAccessReplacement. fixed is false, policy
// unchanged, if there was nothing to fix.
func FixOpenAccessGrant(policy string) (updated string, fixed bool) {
	start, end, ok := findOpenAccessGrant(policy)
	if !ok {
		return policy, false
	}
	return policy[:start] + openAccessReplacement + policy[end:], true
}
