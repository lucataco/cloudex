package node

import (
	"encoding/json"
	"net/netip"
	"reflect"
	"sort"
	"strings"
	"testing"
)

func testHost(t *testing.T) MachineHost {
	t.Helper()
	n, err := NetFor("cx-abc123", netip.MustParsePrefix("10.250.0.16/28"))
	if err != nil {
		t.Fatal(err)
	}
	return MachineHost{NodeID: "nd-abcd1234", Image: "ghcr.io/lucataco/cloudex-base:v0.2.2", Network: n, HostCPU: 4, HostMB: 16000}
}

// The whole body, as Docker gets it. Any change to a machine's flags shows here.
const wantBody = `{
  "Image": "ghcr.io/lucataco/cloudex-base:v0.2.2",
  "Hostname": "cx-abc123",
  "Env": ["CLOUDEX_SANDBOX=cx-abc123", "CLOUDEX_DOCKER=0"],
  "Labels": {"build.cloudex.node": "nd-abcd1234", "build.cloudex.role": "machine", "build.cloudex.sandbox": "cx-abc123"},
  "StopTimeout": 20,
  "HostConfig": {
    "NetworkMode": "cloudex-cx-abc123",
    "Privileged": false,
    "CapDrop": ["NET_RAW", "MKNOD", "SETFCAP"],
    "PidMode": "",
    "IpcMode": "private",
    "UTSMode": "",
    "UsernsMode": "",
    "CgroupnsMode": "private",
    "Runtime": "runc",
    "Tmpfs": {"/run": "rw,nosuid,nodev,noexec,mode=755"},
    "NanoCpus": 1000000000,
    "Memory": 6442450944,
    "MemorySwap": 6442450944,
    "PidsLimit": 4096,
    "OomScoreAdj": 500,
    "RestartPolicy": {"Name": "no"},
    "Dns": ["1.1.1.1", "8.8.8.8"],
    "DnsSearch": ["."],
    "ExtraHosts": ["cloudex.internal:10.250.0.18"],
    "Sysctls": {"net.ipv6.conf.default.disable_ipv6": "1"},
    "LogConfig": {"Type": "json-file", "Config": {"max-file": "2", "max-size": "10m"}},
    "ReadonlyRootfs": false
  },
  "NetworkingConfig": {"EndpointsConfig": {"cloudex-cx-abc123": {"IPAMConfig": {"IPv4Address": "10.250.0.19"}}}}
}`

func asMap(t *testing.T, v any) map[string]any {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	json.Unmarshal(b, &m)
	return m
}

func TestAMachineIsCreatedWithExactlyTheseFlags(t *testing.T) {
	b, err := MachineSpec(MachineRequest{Sandbox: "cx-abc123"}, testHost(t))
	if err != nil {
		t.Fatal(err)
	}
	var want map[string]any
	if err := json.Unmarshal([]byte(wantBody), &want); err != nil {
		t.Fatal(err)
	}
	if got := asMap(t, b); !reflect.DeepEqual(got, want) {
		g, _ := json.MarshalIndent(got, "", "  ")
		t.Fatalf("the create body changed:\n%s", g)
	}
}

func TestUnderSysboxAMachineGetsDockerInside(t *testing.T) {
	h := testHost(t)
	h.Sysbox = true
	b, err := MachineSpec(MachineRequest{Sandbox: "cx-abc123"}, h)
	if err != nil {
		t.Fatal(err)
	}
	if b.HostConfig.Runtime != "sysbox-runc" || !contains(b.Env, "CLOUDEX_DOCKER=1") || b.HostConfig.Privileged || len(b.HostConfig.CapAdd) != 0 {
		t.Fatalf("sysbox body: runtime %s env %v", b.HostConfig.Runtime, b.Env)
	}
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// The keys HostConfig may ever have in a machine's body. Docker reads binds,
// mounts, devices, added capabilities, ports, security options and namespace
// modes from keys that aren't here; a machine never sends them.
var hostConfigKeys = []string{"CapDrop", "CgroupnsMode", "Dns", "DnsSearch", "ExtraHosts", "IpcMode", "LogConfig", "Memory", "MemorySwap",
	"NanoCpus", "NetworkMode", "OomScoreAdj", "PidMode", "PidsLimit", "Privileged", "ReadonlyRootfs", "RestartPolicy", "Runtime",
	"Sysctls", "Tmpfs", "UTSMode", "UsernsMode"}

// Whatever a request carries, nothing in it can add a mount, a capability, a
// device, privileged mode, host networking or a namespace shared with the host.
func TestNothingARequestCarriesWidensAMachine(t *testing.T) {
	hostile := []string{
		"--privileged", "/var/run/docker.sock:/var/run/docker.sock", "host", "SYS_ADMIN", "/dev/sda", "container:cloudex-node",
		`","Privileged":true,"x":"`, "\x00", strings.Repeat("A", 100), "pid=host",
	}
	reqs := []MachineRequest{}
	for _, h := range hostile {
		reqs = append(reqs,
			MachineRequest{Sandbox: "cx-abc123", Env: map[string]string{"X": h}},
			MachineRequest{Sandbox: "cx-abc123", Env: map[string]string{h: "1"}},
			MachineRequest{Sandbox: h},
			MachineRequest{Sandbox: "cx-abc123", Image: h},
		)
	}
	reqs = append(reqs,
		MachineRequest{Sandbox: "cx-abc123", Env: map[string]string{"CLOUDEX_DOCKER": "1"}},
		MachineRequest{Sandbox: "cx-abc123", Env: map[string]string{"CLOUDEX_SANDBOX": "cx-zzz999"}},
		MachineRequest{Sandbox: "cx-abc123", CPUs: 1e9}, MachineRequest{Sandbox: "cx-abc123", CPUs: -1},
		MachineRequest{Sandbox: "cx-abc123", CPUs: 0.1}, MachineRequest{Sandbox: "cx-abc123", MemoryMB: 1 << 30},
		MachineRequest{Sandbox: "cx-abc123", MemoryMB: 16}, MachineRequest{Sandbox: "cx-abc123", Hold: true},
		MachineRequest{Sandbox: "cx-tabc123"}, // a probe's id is not a sandbox's
	)
	ok := 0
	for _, r := range reqs {
		b, err := MachineSpec(r, testHost(t))
		if err != nil {
			continue
		}
		ok++
		m := asMap(t, b)
		hc := m["HostConfig"].(map[string]any)
		keys := []string{}
		for k := range hc {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		if !reflect.DeepEqual(keys, hostConfigKeys) {
			t.Fatalf("%+v: HostConfig has %v", r, keys)
		}
		if hc["Privileged"] != false || hc["NetworkMode"] != "cloudex-cx-abc123" || hc["PidMode"] != "" || hc["IpcMode"] != "private" ||
			hc["UsernsMode"] != "" || hc["UTSMode"] != "" || hc["CgroupnsMode"] != "private" || hc["Runtime"] != "runc" || hc["ReadonlyRootfs"] != false {
			t.Fatalf("%+v: %v", r, hc)
		}
		if !reflect.DeepEqual(hc["CapDrop"], []any{"NET_RAW", "MKNOD", "SETFCAP"}) || !reflect.DeepEqual(hc["Tmpfs"], map[string]any{"/run": "rw,nosuid,nodev,noexec,mode=755"}) {
			t.Fatalf("%+v: %v %v", r, hc["CapDrop"], hc["Tmpfs"])
		}
		for k := range m {
			if k != "Image" && k != "Hostname" && k != "Env" && k != "Labels" && k != "StopTimeout" && k != "HostConfig" && k != "NetworkingConfig" {
				t.Fatalf("%+v: an unexpected key %s", r, k)
			}
		}
		if m["Image"] != "ghcr.io/lucataco/cloudex-base:v0.2.2" || m["Hostname"] != "cx-abc123" {
			t.Fatalf("%+v: image %v hostname %v", r, m["Image"], m["Hostname"])
		}
		env := b.Env
		if !contains(env, "CLOUDEX_SANDBOX=cx-abc123") || !contains(env, "CLOUDEX_DOCKER=0") {
			t.Fatalf("%+v: env %v", r, env)
		}
		for _, e := range env {
			k, _, _ := strings.Cut(e, "=")
			if reservedEnv[k] && e != "CLOUDEX_SANDBOX=cx-abc123" && e != "CLOUDEX_DOCKER=0" && e != "CLOUDEX_HOLD=1" {
				t.Fatalf("%+v: a request set %s", r, e)
			}
		}
	}
	if ok == 0 {
		t.Fatal("no request was accepted")
	}
	// And these are refused outright.
	for _, r := range []MachineRequest{
		{Sandbox: "cx-abc123; --privileged"}, {Sandbox: "host"}, {Sandbox: "cx-tabc123"},
		{Sandbox: "cx-abc123", Env: map[string]string{"--privileged": "1"}}, {Sandbox: "cx-abc123", Env: map[string]string{"X": "\x00"}},
		{Sandbox: "cx-abc123", Env: map[string]string{"CLOUDEX_DOCKER": "1"}}, {Sandbox: "cx-abc123", CPUs: 1e9}, {Sandbox: "cx-abc123", MemoryMB: 1 << 30},
	} {
		if _, err := MachineSpec(r, testHost(t)); err == nil {
			t.Errorf("accepted %+v", r)
		}
	}
}

func TestOnlyTheSelfTestsProbeGetsNetAdmin(t *testing.T) {
	h := testHost(t)
	b, err := ProbeSpec("cx-t0a1b2c", h)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(b.HostConfig.CapAdd, []string{"NET_ADMIN"}) || b.HostConfig.Privileged || b.Labels[LabelRole] != "probe" {
		t.Fatalf("probe: %+v", b.HostConfig)
	}
	if _, err := ProbeSpec("cx-abc123", h); err == nil {
		t.Fatal("a probe with a sandbox's id")
	}
	m, _ := MachineSpec(MachineRequest{Sandbox: "cx-abc123"}, h)
	if m.HostConfig.CapAdd != nil {
		t.Fatal("a machine with added capabilities")
	}
}

func TestSizesDefaultAndStayWithinTheHost(t *testing.T) {
	h := testHost(t)
	b, _ := MachineSpec(MachineRequest{Sandbox: "cx-abc123", CPUs: 2.5, MemoryMB: 2048}, h)
	if b.HostConfig.NanoCPUs != 2_500_000_000 || b.HostConfig.Memory != 2048<<20 || b.HostConfig.MemorySwap != 2048<<20 {
		t.Fatalf("%+v", b.HostConfig)
	}
	h.HostMB = 4000 // a small host: half its memory
	b, _ = MachineSpec(MachineRequest{Sandbox: "cx-abc123"}, h)
	if b.HostConfig.Memory != 2000<<20 {
		t.Fatalf("default on a small host: %d", b.HostConfig.Memory>>20)
	}
	if _, err := MachineSpec(MachineRequest{Sandbox: "cx-abc123", CPUs: 5}, h); err == nil {
		t.Fatal("more CPUs than the host has")
	}
}

// An exec can't ask Docker for privileges either: its body has no Privileged.
func TestAnExecIsNeverPrivileged(t *testing.T) {
	m := asMap(t, ExecConfig{Cmd: []string{"sh"}, User: "0"})
	for _, k := range []string{"Privileged", "Detach", "DetachKeys"} {
		if _, ok := m[k]; ok {
			t.Fatalf("an exec body with %s", k)
		}
	}
}

func TestImagesOutsideTheListAreRefused(t *testing.T) {
	for ref, ok := range map[string]bool{
		"ghcr.io/lucataco/cloudex-base:v0.2.2":                                   true,
		"ghcr.io/lucataco/cloudex-base:v0.2.2@sha256:" + strings.Repeat("a", 64): true,
		"ghcr.io/lucataco/cloudex-base@sha256:" + strings.Repeat("a", 64):        true,
		"ghcr.io/lucataco/cloudex-base":                                          true,
		"ghcr.io/lucataco/cloudex-base-evil:v1":                                  false,
		"ghcr.io/evil/cloudex-base:v1":                                           false,
		"docker.io/library/ubuntu:24.04":                                         false,
		"ghcr.io/lucataco/cloudex-base:v1 --privileged":                          false,
		"evil.example/ghcr.io/lucataco/cloudex-base:v1":                          false,
		"": false,
	} {
		if err := ImageAllowed(ref, nil); (err == nil) != ok {
			t.Errorf("%q: %v", ref, err)
		}
	}
	if err := ImageAllowed("registry.lan:5000/me/base:1", []string{"registry.lan:5000/me/base"}); err != nil {
		t.Error(err)
	}
}
