package plasma

import (
	"fmt"
	"net/netip"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Link operations act on one end of a link, named "<vm>:<ethN>": every link
// NIC is a tap on the host, so its traffic can be mirrored or impaired there
// with tc, which until now a client could only do with sudo on the engine.

// LinkFilter impairs the frames delivered TO a link NIC (the tap's egress,
// host to guest). A VM-to-VM link is impaired both ways by also setting
// BothEnds, which applies the same filter to the peer's NIC.
type LinkFilter struct {
	// Delay and Jitter are Go durations ("100ms", "1.5s").
	Delay  string `json:"delay,omitempty"`
	Jitter string `json:"jitter,omitempty"`
	// Loss is a percentage, 0-100.
	Loss float64 `json:"loss,omitempty"`
	// Drop loses every frame: the link is cut.
	Drop     bool `json:"drop,omitempty"`
	BothEnds bool `json:"both_ends,omitempty"`
}

// MirrorRequest copies every frame crossing a link NIC, both directions, out
// of another tap in the same lab, typically a `host:` endpoint of a capture
// VM.
type MirrorRequest struct {
	To string `json:"to"`
}

const maxNetemDelay = time.Minute

// netemArgs turns a filter into tc arguments. Every value is parsed and then
// re-formatted, so nothing a client sent reaches tc's command line verbatim.
func (f LinkFilter) netemArgs() ([]string, error) {
	var args []string
	if f.Delay != "" {
		d, err := time.ParseDuration(f.Delay)
		if err != nil || d < 0 || d > maxNetemDelay {
			return nil, fmt.Errorf("delay %q: want a duration between 0 and %s", f.Delay, maxNetemDelay)
		}
		args = append(args, "delay", strconv.FormatInt(d.Microseconds(), 10)+"us")
		if f.Jitter != "" {
			j, err := time.ParseDuration(f.Jitter)
			if err != nil || j < 0 || j > d {
				return nil, fmt.Errorf("jitter %q: want a duration between 0 and the delay", f.Jitter)
			}
			args = append(args, strconv.FormatInt(j.Microseconds(), 10)+"us")
		}
	} else if f.Jitter != "" {
		return nil, fmt.Errorf("jitter needs a delay")
	}
	loss := f.Loss
	if f.Drop {
		loss = 100
	}
	if loss < 0 || loss > 100 {
		return nil, fmt.Errorf("loss %v: want a percentage between 0 and 100", f.Loss)
	}
	if loss > 0 {
		args = append(args, "loss", strconv.FormatFloat(loss, 'f', -1, 64)+"%")
	}
	if len(args) == 0 {
		return nil, fmt.Errorf("filter is empty: set delay, loss or drop (DELETE clears a filter)")
	}
	return args, nil
}

func runTC(args ...string) error {
	out, err := exec.Command("tc", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("tc %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

func applyFilter(tap string, f LinkFilter) error {
	args, err := f.netemArgs()
	if err != nil {
		return err
	}
	return runTC(append([]string{"qdisc", "replace", "dev", tap, "root", "netem"}, args...)...)
}

// clearFilter restores the tap's default qdisc. Clearing a tap with no
// filter is not an error.
func clearFilter(tap string) error {
	err := runTC("qdisc", "del", "dev", tap, "root")
	if err != nil && (strings.Contains(err.Error(), "No such file") ||
		strings.Contains(err.Error(), "handle of zero") || strings.Contains(err.Error(), "Invalid handle")) {
		return nil
	}
	return err
}

// mirrorPref is the tc filter priority plasma's mirror uses on a tap; drop
// rules take 1..maxDropRules, so the two never collide on one hook.
const mirrorPref = "100"

func ensureClsact(tap string) error {
	if err := runTC("qdisc", "add", "dev", tap, "clsact"); err != nil && !strings.Contains(err.Error(), "File exists") {
		return err
	}
	return nil
}

// tcGone is a delete of something already absent.
func tcGone(err error) bool {
	if err == nil {
		return true
	}
	for _, s := range []string{"No such file", "Invalid handle", "Cannot find specified filter chain", "Filter with specified priority/protocol not found"} {
		if strings.Contains(err.Error(), s) {
			return true
		}
	}
	return false
}

// applyMirror copies frames in both directions on src out of dst. clsact
// gives a tap an ingress hook (frames from the guest) and an egress hook
// (frames to it); a matchall filter on each mirrors to dst.
func applyMirror(src, dst string) error {
	if err := ensureClsact(src); err != nil {
		return err
	}
	for _, hook := range []string{"ingress", "egress"} {
		if err := runTC("filter", "replace", "dev", src, hook, "pref", mirrorPref, "handle", "1",
			"matchall", "action", "mirred", "egress", "mirror", "dev", dst); err != nil {
			return err
		}
	}
	return nil
}

// clearMirror removes only the mirror's filters: drop rules may share the
// tap's clsact.
func clearMirror(src string) error {
	for _, hook := range []string{"ingress", "egress"} {
		if err := runTC("filter", "del", "dev", src, hook, "pref", mirrorPref); !tcGone(err) {
			return err
		}
	}
	return nil
}

// PacketCount is what a tc filter's action has handled.
type PacketCount struct {
	Bytes   int64 `json:"bytes"`
	Packets int64 `json:"packets"`
}

// MirrorStatus is GET .../links/{link}/mirror: the mirror's own counters, per
// direction (ingress = from the guest, egress = to it). A mirror that carried
// nothing in a direction saw no traffic there.
type MirrorStatus struct {
	Link    string      `json:"link"`
	To      string      `json:"to"`
	Ingress PacketCount `json:"ingress"`
	Egress  PacketCount `json:"egress"`
}

var sentPattern = regexp.MustCompile(`Sent (\d+) bytes (\d+) pkt`)

// filterCount reads the first action counter of a tap's filter.
func filterCount(tap, hook, pref string) PacketCount {
	out, err := exec.Command("tc", "-s", "filter", "show", "dev", tap, hook, "pref", pref).CombinedOutput()
	if err != nil {
		return PacketCount{}
	}
	m := sentPattern.FindSubmatch(out)
	if m == nil {
		return PacketCount{}
	}
	b, _ := strconv.ParseInt(string(m[1]), 10, 64)
	p, _ := strconv.ParseInt(string(m[2]), 10, 64)
	return PacketCount{Bytes: b, Packets: p}
}

// Drop rules lose chosen frames on their way to a link NIC: tc u32 filters on
// the tap's egress, the way integrationv2's fault injector drops one
// handshake datagram. Rules are alternatives: a frame matching any one is
// dropped.

// ByteMatch matches one byte at an offset from the start of the IP header.
type ByteMatch struct {
	Offset int `json:"offset"`
	Value  int `json:"value"`
	// Mask defaults to 0xff.
	Mask int `json:"mask,omitempty"`
}

// DropRule is one u32 filter. Every field set must match; at least one must
// be set (cutting a link entirely is `impair --drop`).
type DropRule struct {
	// Protocol is the IP protocol number (6 TCP, 17 UDP, 1 ICMP).
	Protocol int         `json:"protocol,omitempty"`
	Src      string      `json:"src,omitempty"`
	Dst      string      `json:"dst,omitempty"`
	Sport    int         `json:"sport,omitempty"`
	Dport    int         `json:"dport,omitempty"`
	Bytes    []ByteMatch `json:"bytes,omitempty"`
}

// DropRequest arms drop rules on a link NIC. With Once, the engine removes
// them as soon as one frame was dropped (or after Timeout, default 30s),
// watching the counters itself so no client round trip lets a second frame
// through. Without it they hold until cleared.
type DropRequest struct {
	Rules   []DropRule `json:"rules"`
	Once    bool       `json:"once,omitempty"`
	Timeout string     `json:"timeout,omitempty"`
}

// DropStatus reports drop rules: Armed while installed, Done once a Once
// request has finished, and the frames dropped so far (or in total).
type DropStatus struct {
	Link    string `json:"link"`
	Armed   bool   `json:"armed"`
	Once    bool   `json:"once,omitempty"`
	Done    bool   `json:"done,omitempty"`
	Dropped int64  `json:"dropped"`
}

const maxDropRules = 8

const (
	defaultDropTimeout = 30 * time.Second
	maxDropTimeout     = 10 * time.Minute
)

func (q DropRequest) timeout() (time.Duration, error) {
	if q.Timeout == "" {
		return defaultDropTimeout, nil
	}
	d, err := time.ParseDuration(q.Timeout)
	if err != nil || d <= 0 || d > maxDropTimeout {
		return 0, fmt.Errorf("timeout %q: want a duration up to %s", q.Timeout, maxDropTimeout)
	}
	return d, nil
}

func (q DropRequest) validate() error {
	if len(q.Rules) == 0 || len(q.Rules) > maxDropRules {
		return fmt.Errorf("want 1 to %d rules, got %d", maxDropRules, len(q.Rules))
	}
	for i, r := range q.Rules {
		if _, err := r.u32Args(); err != nil {
			return fmt.Errorf("rules[%d]: %w", i, err)
		}
	}
	_, err := q.timeout()
	return err
}

// u32Args is the rule as tc u32 matches. As with netem, every value is parsed
// and re-formatted.
func (r DropRule) u32Args() ([]string, error) {
	var m []string
	if r.Protocol != 0 {
		if r.Protocol < 0 || r.Protocol > 255 {
			return nil, fmt.Errorf("protocol %d: want 0-255", r.Protocol)
		}
		m = append(m, "match", "ip", "protocol", strconv.Itoa(r.Protocol), "0xff")
	}
	for _, a := range []struct{ name, v string }{{"src", r.Src}, {"dst", r.Dst}} {
		if a.v == "" {
			continue
		}
		p, err := netip.ParsePrefix(a.v)
		if err != nil {
			addr, aerr := netip.ParseAddr(a.v)
			if aerr != nil {
				return nil, fmt.Errorf("%s %q: want an IPv4 address or prefix", a.name, a.v)
			}
			p = netip.PrefixFrom(addr, 32)
		}
		if !p.Addr().Is4() {
			return nil, fmt.Errorf("%s %q: want IPv4", a.name, a.v)
		}
		m = append(m, "match", "ip", a.name, p.Masked().String())
	}
	for _, port := range []struct {
		name string
		v    int
	}{{"sport", r.Sport}, {"dport", r.Dport}} {
		if port.v == 0 {
			continue
		}
		if port.v < 0 || port.v > 65535 {
			return nil, fmt.Errorf("%s %d: want 1-65535", port.name, port.v)
		}
		m = append(m, "match", "ip", port.name, strconv.Itoa(port.v), "0xffff")
	}
	for _, b := range r.Bytes {
		mask := b.Mask
		if mask == 0 {
			mask = 0xff
		}
		if b.Offset < 0 || b.Offset > 1500 || b.Value < 0 || b.Value > 255 || mask < 0 || mask > 255 {
			return nil, fmt.Errorf("byte match %+v: want offset 0-1500, value and mask 0-255", b)
		}
		m = append(m, "match", "u8", fmt.Sprintf("0x%x", b.Value), fmt.Sprintf("0x%x", mask), "at", strconv.Itoa(b.Offset))
	}
	if len(m) == 0 {
		return nil, fmt.Errorf("a rule needs at least one match")
	}
	return append(append([]string{"protocol", "ip", "u32"}, m...), "action", "drop"), nil
}

func installDrop(tap string, rules []DropRule) error {
	if err := ensureClsact(tap); err != nil {
		return err
	}
	clearDrop(tap)
	for i, r := range rules {
		args, err := r.u32Args()
		if err == nil {
			err = runTC(append([]string{"filter", "add", "dev", tap, "egress", "pref", strconv.Itoa(i + 1)}, args...)...)
		}
		if err != nil {
			clearDrop(tap)
			return err
		}
	}
	return nil
}

func dropCount(tap string) int64 {
	var n int64
	for pref := 1; pref <= maxDropRules; pref++ {
		n += filterCount(tap, "egress", strconv.Itoa(pref)).Packets
	}
	return n
}

func clearDrop(tap string) {
	for pref := 1; pref <= maxDropRules; pref++ {
		runTC("filter", "del", "dev", tap, "egress", "pref", strconv.Itoa(pref))
	}
}
