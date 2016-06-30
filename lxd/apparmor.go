package main

import (
	"crypto/sha256"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"os/exec"
	"path"
	"strings"

	"github.com/lxc/lxd/shared"

	log "gopkg.in/inconshreveable/log15.v2"
)

const (
	APPARMOR_CMD_LOAD   = "r"
	APPARMOR_CMD_UNLOAD = "R"
	APPARMOR_CMD_PARSE  = "Q"
)

var aaPath = shared.VarPath("security", "apparmor")

const NESTING_AA_PROFILE = `
  pivot_root,
  mount /var/lib/lxd/shmounts/ -> /var/lib/lxd/shmounts/,
  mount none -> /var/lib/lxd/shmounts/,
  mount fstype=proc -> /usr/lib/*/lxc/**,
  mount fstype=sysfs -> /usr/lib/*/lxc/**,
  mount options=(rw,bind),
  mount options=(rw,rbind),
  deny /dev/.lxd/proc/** rw,
  deny /dev/.lxd/sys/** rw,
  mount options=(rw,make-rshared),

  # there doesn't seem to be a way to ask for:
  # mount options=(ro,nosuid,nodev,noexec,remount,bind),
  # as we always get mount to $cdir/proc/sys with those flags denied
  # So allow all mounts until that is straightened out:
  mount,
  mount options=bind /var/lib/lxd/shmounts/** -> /var/lib/lxd/**,
  # lxc-container-default-with-nesting also inherited these
  # from start-container, and seems to need them.
  ptrace,
  signal,
`

const DEFAULT_AA_PROFILE = `
#include <tunables/global>
profile "%s" flags=(attach_disconnected,mediate_deleted) {
  network,
  capability,
  file,
  umount,

  # dbus, signal, ptrace and unix are only supported by recent apparmor
  # versions. Comment them if the apparmor parser doesn't recognize them.

  # This also needs additional rules to reach outside of the container via
  # DBus, so just let all of DBus within the container.
  dbus,

  # Allow us to receive signals from anywhere. Note: if per-container profiles
  # are supported, for container isolation this should be changed to something
  # like:
  #   signal (receive) peer=unconfined,
  #   signal (receive) peer=/usr/bin/lxc-start,
  signal (receive),

  # Allow us to send signals to ourselves
  signal peer=@{profile_name},

  # Allow other processes to read our /proc entries, futexes, perf tracing and
  # kcmp for now (they will need 'read' in the first place). Administrators can
  # override with:
  #   deny ptrace (readby) ...
  ptrace (readby),

  # Allow other processes to trace us by default (they will need 'trace' in
  # the first place). Administrators can override with:
  #   deny ptrace (tracedby) ...
  ptrace (tracedby),

  # Allow us to ptrace ourselves
  ptrace peer=@{profile_name},

  # Allow receive via unix sockets from anywhere. Note: if per-container
  # profiles are supported, for container isolation this should be changed to
  # something like:
  #   unix (receive) peer=(label=unconfined),
  unix (receive),

  # Allow all unix in the container
  unix peer=(label=@{profile_name}),

  # ignore DENIED message on / remount
  deny mount options=(ro, remount) -> /,
  deny mount options=(ro, remount, silent) -> /,

  # allow tmpfs mounts everywhere
  mount fstype=tmpfs,

  # allow hugetlbfs mounts everywhere
  mount fstype=hugetlbfs,

  # allow mqueue mounts everywhere
  mount fstype=mqueue,

  # allow fuse mounts everywhere
  mount fstype=fuse,
  mount fstype=fuse.*,

  # deny access under /proc/bus to avoid e.g. messing with pci devices directly
  deny @{PROC}/bus/** wklx,

  # deny writes in /proc/sys/fs but allow binfmt_misc to be mounted
  mount fstype=binfmt_misc -> /proc/sys/fs/binfmt_misc/,
  deny @{PROC}/sys/fs/** wklx,

  # allow efivars to be mounted, writing to it will be blocked though
  mount fstype=efivarfs -> /sys/firmware/efi/efivars/,

  # block some other dangerous paths
  deny @{PROC}/kcore rwklx,
  deny @{PROC}/kmem rwklx,
  deny @{PROC}/mem rwklx,
  deny @{PROC}/sysrq-trigger rwklx,

  # deny writes in /sys except for /sys/fs/cgroup, also allow
  # fusectl, securityfs and debugfs to be mounted there (read-only)
  mount fstype=fusectl -> /sys/fs/fuse/connections/,
  mount fstype=securityfs -> /sys/kernel/security/,
  mount fstype=debugfs -> /sys/kernel/debug/,
  deny mount fstype=debugfs -> /var/lib/ureadahead/debugfs/,
  mount fstype=proc -> /proc/,
  mount fstype=sysfs -> /sys/,
  mount options=(rw, nosuid, nodev, noexec, remount) -> /sys/,
  deny /sys/firmware/efi/efivars/** rwklx,
  deny /sys/kernel/security/** rwklx,
  mount options=(move) /sys/fs/cgroup/cgmanager/ -> /sys/fs/cgroup/cgmanager.lower/,
  mount options=(ro, nosuid, nodev, noexec, remount, strictatime) -> /sys/fs/cgroup/,

  # deny reads from debugfs
  deny /sys/kernel/debug/{,**} rwklx,

  # allow paths to be made slave, shared, private or unbindable
  # FIXME: This currently doesn't work due to the apparmor parser treating those as allowing all mounts.
#  mount options=(rw,make-slave) -> **,
#  mount options=(rw,make-rslave) -> **,
#  mount options=(rw,make-shared) -> **,
#  mount options=(rw,make-rshared) -> **,
#  mount options=(rw,make-private) -> **,
#  mount options=(rw,make-rprivate) -> **,
#  mount options=(rw,make-unbindable) -> **,
#  mount options=(rw,make-runbindable) -> **,

  # allow bind-mounts of anything except /proc, /sys and /dev
  mount options=(rw,bind) /[^spd]*{,/**},
  mount options=(rw,bind) /d[^e]*{,/**},
  mount options=(rw,bind) /de[^v]*{,/**},
  mount options=(rw,bind) /dev/.[^l]*{,/**},
  mount options=(rw,bind) /dev/.l[^x]*{,/**},
  mount options=(rw,bind) /dev/.lx[^c]*{,/**},
  mount options=(rw,bind) /dev/.lxc?*{,/**},
  mount options=(rw,bind) /dev/[^.]*{,/**},
  mount options=(rw,bind) /dev?*{,/**},
  mount options=(rw,bind) /p[^r]*{,/**},
  mount options=(rw,bind) /pr[^o]*{,/**},
  mount options=(rw,bind) /pro[^c]*{,/**},
  mount options=(rw,bind) /proc?*{,/**},
  mount options=(rw,bind) /s[^y]*{,/**},
  mount options=(rw,bind) /sy[^s]*{,/**},
  mount options=(rw,bind) /sys?*{,/**},

  # allow moving mounts except for /proc, /sys and /dev
  mount options=(rw,move) /[^spd]*{,/**},
  mount options=(rw,move) /d[^e]*{,/**},
  mount options=(rw,move) /de[^v]*{,/**},
  mount options=(rw,move) /dev/.[^l]*{,/**},
  mount options=(rw,move) /dev/.l[^x]*{,/**},
  mount options=(rw,move) /dev/.lx[^c]*{,/**},
  mount options=(rw,move) /dev/.lxc?*{,/**},
  mount options=(rw,move) /dev/[^.]*{,/**},
  mount options=(rw,move) /dev?*{,/**},
  mount options=(rw,move) /p[^r]*{,/**},
  mount options=(rw,move) /pr[^o]*{,/**},
  mount options=(rw,move) /pro[^c]*{,/**},
  mount options=(rw,move) /proc?*{,/**},
  mount options=(rw,move) /s[^y]*{,/**},
  mount options=(rw,move) /sy[^s]*{,/**},
  mount options=(rw,move) /sys?*{,/**},

  # generated by: lxc-generate-aa-rules.py container-rules.base
  deny /proc/sys/[^kn]*{,/**} wklx,
  deny /proc/sys/k[^e]*{,/**} wklx,
  deny /proc/sys/ke[^r]*{,/**} wklx,
  deny /proc/sys/ker[^n]*{,/**} wklx,
  deny /proc/sys/kern[^e]*{,/**} wklx,
  deny /proc/sys/kerne[^l]*{,/**} wklx,
  deny /proc/sys/kernel/[^smhd]*{,/**} wklx,
  deny /proc/sys/kernel/d[^o]*{,/**} wklx,
  deny /proc/sys/kernel/do[^m]*{,/**} wklx,
  deny /proc/sys/kernel/dom[^a]*{,/**} wklx,
  deny /proc/sys/kernel/doma[^i]*{,/**} wklx,
  deny /proc/sys/kernel/domai[^n]*{,/**} wklx,
  deny /proc/sys/kernel/domain[^n]*{,/**} wklx,
  deny /proc/sys/kernel/domainn[^a]*{,/**} wklx,
  deny /proc/sys/kernel/domainna[^m]*{,/**} wklx,
  deny /proc/sys/kernel/domainnam[^e]*{,/**} wklx,
  deny /proc/sys/kernel/domainname?*{,/**} wklx,
  deny /proc/sys/kernel/h[^o]*{,/**} wklx,
  deny /proc/sys/kernel/ho[^s]*{,/**} wklx,
  deny /proc/sys/kernel/hos[^t]*{,/**} wklx,
  deny /proc/sys/kernel/host[^n]*{,/**} wklx,
  deny /proc/sys/kernel/hostn[^a]*{,/**} wklx,
  deny /proc/sys/kernel/hostna[^m]*{,/**} wklx,
  deny /proc/sys/kernel/hostnam[^e]*{,/**} wklx,
  deny /proc/sys/kernel/hostname?*{,/**} wklx,
  deny /proc/sys/kernel/m[^s]*{,/**} wklx,
  deny /proc/sys/kernel/ms[^g]*{,/**} wklx,
  deny /proc/sys/kernel/msg*/** wklx,
  deny /proc/sys/kernel/s[^he]*{,/**} wklx,
  deny /proc/sys/kernel/se[^m]*{,/**} wklx,
  deny /proc/sys/kernel/sem*/** wklx,
  deny /proc/sys/kernel/sh[^m]*{,/**} wklx,
  deny /proc/sys/kernel/shm*/** wklx,
  deny /proc/sys/kernel?*{,/**} wklx,
  deny /proc/sys/n[^e]*{,/**} wklx,
  deny /proc/sys/ne[^t]*{,/**} wklx,
  deny /proc/sys/net?*{,/**} wklx,
  deny /sys/[^fdc]*{,/**} wklx,
  deny /sys/c[^l]*{,/**} wklx,
  deny /sys/cl[^a]*{,/**} wklx,
  deny /sys/cla[^s]*{,/**} wklx,
  deny /sys/clas[^s]*{,/**} wklx,
  deny /sys/class/[^n]*{,/**} wklx,
  deny /sys/class/n[^e]*{,/**} wklx,
  deny /sys/class/ne[^t]*{,/**} wklx,
  deny /sys/class/net?*{,/**} wklx,
  deny /sys/class?*{,/**} wklx,
  deny /sys/d[^e]*{,/**} wklx,
  deny /sys/de[^v]*{,/**} wklx,
  deny /sys/dev[^i]*{,/**} wklx,
  deny /sys/devi[^c]*{,/**} wklx,
  deny /sys/devic[^e]*{,/**} wklx,
  deny /sys/device[^s]*{,/**} wklx,
  deny /sys/devices/[^v]*{,/**} wklx,
  deny /sys/devices/v[^i]*{,/**} wklx,
  deny /sys/devices/vi[^r]*{,/**} wklx,
  deny /sys/devices/vir[^t]*{,/**} wklx,
  deny /sys/devices/virt[^u]*{,/**} wklx,
  deny /sys/devices/virtu[^a]*{,/**} wklx,
  deny /sys/devices/virtua[^l]*{,/**} wklx,
  deny /sys/devices/virtual/[^n]*{,/**} wklx,
  deny /sys/devices/virtual/n[^e]*{,/**} wklx,
  deny /sys/devices/virtual/ne[^t]*{,/**} wklx,
  deny /sys/devices/virtual/net?*{,/**} wklx,
  deny /sys/devices/virtual?*{,/**} wklx,
  deny /sys/devices?*{,/**} wklx,
  deny /sys/f[^s]*{,/**} wklx,
  deny /sys/fs/[^c]*{,/**} wklx,
  deny /sys/fs/c[^g]*{,/**} wklx,
  deny /sys/fs/cg[^r]*{,/**} wklx,
  deny /sys/fs/cgr[^o]*{,/**} wklx,
  deny /sys/fs/cgro[^u]*{,/**} wklx,
  deny /sys/fs/cgrou[^p]*{,/**} wklx,
  deny /sys/fs/cgroup?*{,/**} wklx,
  deny /sys/fs?*{,/**} wklx,

  # Special exception for cgroup namespaces
%s

  # user input raw.apparmor below here
  %s

  # nesting support goes here if needed
%s
  change_profile -> "%s",
}`

func AAProfileFull(c container) string {
	lxddir := shared.VarPath("")
	if len(c.Name())+len(lxddir)+7 >= 253 {
		hash := sha256.New()
		io.WriteString(hash, lxddir)
		lxddir = fmt.Sprintf("%x", hash.Sum(nil))
	}

	return fmt.Sprintf("lxd-%s_<%s>", c.Name(), lxddir)
}

func AAProfileShort(c container) string {
	return fmt.Sprintf("lxd-%s", c.Name())
}

func AAProfileCgns() string {
	if shared.PathExists("/proc/self/ns/cgroup") {
		return "  mount fstype=cgroup -> /sys/fs/cgroup/**,"
	}
	return ""
}

// getProfileContent generates the apparmor profile template from the given
// container. This includes the stock lxc includes as well as stuff from
// raw.apparmor.
func getAAProfileContent(c container) string {
	rawApparmor, ok := c.ExpandedConfig()["raw.apparmor"]
	if !ok {
		rawApparmor = ""
	}

	nesting := ""
	if c.IsNesting() {
		nesting = NESTING_AA_PROFILE
	}

	return fmt.Sprintf(DEFAULT_AA_PROFILE, AAProfileFull(c), AAProfileCgns(), rawApparmor, nesting, AAProfileFull(c))
}

func runApparmor(command string, c container) error {
	if !aaAvailable {
		return nil
	}

	cmd := exec.Command("apparmor_parser", []string{
		fmt.Sprintf("-%sWL", command),
		path.Join(aaPath, "cache"),
		path.Join(aaPath, "profiles", AAProfileShort(c)),
	}...)

	output, err := cmd.CombinedOutput()
	if err != nil {
		shared.Log.Error("Running apparmor",
			log.Ctx{"action": command, "output": string(output), "err": err})
	}

	return err
}

// Ensure that the container's policy is loaded into the kernel so the
// container can boot.
func AALoadProfile(c container) error {
	if !aaAdmin {
		return nil
	}

	/* In order to avoid forcing a profile parse (potentially slow) on
	 * every container start, let's use apparmor's binary policy cache,
	 * which checks mtime of the files to figure out if the policy needs to
	 * be regenerated.
	 *
	 * Since it uses mtimes, we shouldn't just always write out our local
	 * apparmor template; instead we should check to see whether the
	 * template is the same as ours. If it isn't we should write our
	 * version out so that the new changes are reflected and we definitely
	 * force a recompile.
	 */
	profile := path.Join(aaPath, "profiles", AAProfileShort(c))
	content, err := ioutil.ReadFile(profile)
	if err != nil && !os.IsNotExist(err) {
		return err
	}

	updated := getAAProfileContent(c)

	if string(content) != string(updated) {
		if err := os.MkdirAll(path.Join(aaPath, "cache"), 0700); err != nil {
			return err
		}

		if err := os.MkdirAll(path.Join(aaPath, "profiles"), 0700); err != nil {
			return err
		}

		if err := ioutil.WriteFile(profile, []byte(updated), 0600); err != nil {
			return err
		}
	}

	return runApparmor(APPARMOR_CMD_LOAD, c)
}

// Ensure that the container's policy is unloaded to free kernel memory. This
// does not delete the policy from disk or cache.
func AAUnloadProfile(c container) error {
	if !aaAdmin {
		return nil
	}

	return runApparmor(APPARMOR_CMD_UNLOAD, c)
}

// Parse the profile without loading it into the kernel.
func AAParseProfile(c container) error {
	if !aaAvailable {
		return nil
	}

	return runApparmor(APPARMOR_CMD_PARSE, c)
}

// Delete the policy from cache/disk.
func AADeleteProfile(c container) {
	if !aaAdmin {
		return
	}

	/* It's ok if these deletes fail: if the container was never started,
	 * we'll have never written a profile or cached it.
	 */
	os.Remove(path.Join(aaPath, "cache", AAProfileShort(c)))
	os.Remove(path.Join(aaPath, "profiles", AAProfileShort(c)))
}

// What's current apparmor profile
func aaProfile() string {
	contents, err := ioutil.ReadFile("/proc/self/attr/current")
	if err == nil {
		return strings.TrimSpace(string(contents))
	}
	return ""
}
