/* Vuls - Vulnerability Scanner
Copyright (C) 2016  Future Corporation , Japan.

This program is free software: you can redistribute it and/or modify
it under the terms of the GNU General Public License as published by
the Free Software Foundation, either version 3 of the License, or
(at your option) any later version.

This program is distributed in the hope that it will be useful,
but WITHOUT ANY WARRANTY; without even the implied warranty of
MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
GNU General Public License for more details.

You should have received a copy of the GNU General Public License
along with this program.  If not, see <http://www.gnu.org/licenses/>.
*/

package scan

import (
	"bufio"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/future-architect/vuls/cache"
	"github.com/future-architect/vuls/config"
	"github.com/future-architect/vuls/models"
	"github.com/future-architect/vuls/util"
	version "github.com/knqyf263/go-deb-version"
)

// inherit OsTypeInterface
type debian struct {
	base
}

// NewDebian is constructor
func newDebian(c config.ServerInfo) *debian {
	d := &debian{
		base: base{
			osPackages: osPackages{
				Packages:  models.Packages{},
				VulnInfos: models.VulnInfos{},
			},
		},
	}
	d.log = util.NewCustomLogger(c)
	d.setServerInfo(c)
	return d
}

// Ubuntu, Debian, Raspbian
// https://github.com/serverspec/specinfra/blob/master/lib/specinfra/helper/detect_os/debian.rb
func detectDebian(c config.ServerInfo) (itsMe bool, deb osTypeInterface, err error) {
	deb = newDebian(c)

	if r := exec(c, "ls /etc/debian_version", noSudo); !r.isSuccess() {
		if r.Error != nil {
			return false, deb, nil
		}
		if r.ExitStatus == 255 {
			return false, deb, fmt.Errorf("Unable to connect via SSH. Scan with -vvv option to print SSH debugging messages and check SSH settings. If you have never SSH to the host to be scanned, SSH to the host before scanning in order to add the HostKey. %s@%s port: %s\n%s", c.User, c.Host, c.Port, r)
		}
		util.Log.Debugf("Not Debian like Linux. %s", r)
		return false, deb, nil
	}

	// Raspbian
	// lsb_release in Raspbian Jessie returns 'Distributor ID: Raspbian'.
	// However, lsb_release in Raspbian Wheezy returns 'Distributor ID: Debian'.
	if r := exec(c, "cat /etc/issue", noSudo); r.isSuccess() {
		//  e.g.
		//  Raspbian GNU/Linux 7 \n \l
		result := strings.Fields(r.Stdout)
		if len(result) > 2 && result[0] == config.Raspbian {
			distro := strings.ToLower(trim(result[0]))
			deb.setDistro(distro, trim(result[2]))
			return true, deb, nil
		}
	}

	if r := exec(c, "lsb_release -ir", noSudo); r.isSuccess() {
		//  e.g.
		//  root@fa3ec524be43:/# lsb_release -ir
		//  Distributor ID:	Ubuntu
		//  Release:	14.04
		re := regexp.MustCompile(`(?s)^Distributor ID:\s*(.+?)\n*Release:\s*(.+?)$`)
		result := re.FindStringSubmatch(trim(r.Stdout))

		if len(result) == 0 {
			deb.setDistro("debian/ubuntu", "unknown")
			util.Log.Warnf(
				"Unknown Debian/Ubuntu version. lsb_release -ir: %s", r)
		} else {
			distro := strings.ToLower(trim(result[1]))
			deb.setDistro(distro, trim(result[2]))
		}
		return true, deb, nil
	}

	if r := exec(c, "cat /etc/lsb-release", noSudo); r.isSuccess() {
		//  e.g.
		//  DISTRIB_ID=Ubuntu
		//  DISTRIB_RELEASE=14.04
		//  DISTRIB_CODENAME=trusty
		//  DISTRIB_DESCRIPTION="Ubuntu 14.04.2 LTS"
		re := regexp.MustCompile(`(?s)^DISTRIB_ID=(.+?)\n*DISTRIB_RELEASE=(.+?)\n.*$`)
		result := re.FindStringSubmatch(trim(r.Stdout))
		if len(result) == 0 {
			util.Log.Warnf(
				"Unknown Debian/Ubuntu. cat /etc/lsb-release: %s", r)
			deb.setDistro("debian/ubuntu", "unknown")
		} else {
			distro := strings.ToLower(trim(result[1]))
			deb.setDistro(distro, trim(result[2]))
		}
		return true, deb, nil
	}

	// Debian
	cmd := "cat /etc/debian_version"
	if r := exec(c, cmd, noSudo); r.isSuccess() {
		deb.setDistro(config.Debian, trim(r.Stdout))
		return true, deb, nil
	}

	util.Log.Debugf("Not Debian like Linux: %s", c.ServerName)
	return false, deb, nil
}

func trim(str string) string {
	return strings.TrimSpace(str)
}

func (o *debian) checkScanMode() error {
	return nil
}

func (o *debian) checkIfSudoNoPasswd() error {
	if o.getServerInfo().Mode.IsFast() {
		o.log.Infof("sudo ... No need")
		return nil
	}

	cmds := []string{
		"checkrestart",
		"stat /proc/1/exe",
	}

	if !o.getServerInfo().Mode.IsOffline() {
		cmds = append(cmds, "apt-get update")
	}

	for _, cmd := range cmds {
		cmd = util.PrependProxyEnv(cmd)
		o.log.Infof("Checking... sudo %s", cmd)
		r := o.exec(cmd, sudo)
		if !r.isSuccess() {
			o.log.Errorf("sudo error on %s", r)
			return fmt.Errorf("Failed to sudo: %s", r)
		}
	}

	initName, err := o.detectInitSystem()
	if initName == upstart && err == nil {
		cmd := util.PrependProxyEnv("initctl status --help")
		o.log.Infof("Checking... sudo %s", cmd)
		r := o.exec(cmd, sudo)
		if !r.isSuccess() {
			o.log.Errorf("sudo error on %s", r)
			return fmt.Errorf("Failed to sudo: %s", r)
		}
	}

	o.log.Infof("Sudo... Pass")
	return nil
}

type dep struct {
	packName      string
	required      bool
	logFunc       func(string, ...interface{})
	additionalMsg string
}

func (o *debian) checkDeps() error {
	deps := []dep{}
	if o.getServerInfo().Mode.IsDeep() || o.getServerInfo().Mode.IsFastRoot() {
		// checkrestart
		deps = append(deps, dep{
			packName: "debian-goodies",
			required: true,
			logFunc:  o.log.Errorf,
		})
	}

	if o.Distro.Family == config.Debian {
		// https://askubuntu.com/a/742844
		if !o.ServerInfo.IsContainer() {
			deps = append(deps, dep{
				packName:      "reboot-notifier",
				required:      false,
				logFunc:       o.log.Warnf,
				additionalMsg: ". Install it if you want to detect whether not rebooted after kernel update. To install `reboot-notifier` on Debian, see https://feeding.cloud.geek.nz/posts/introducing-reboot-notifier/",
			})
		}

		// Changelogs will be fetched only in deep scan mode
		if o.getServerInfo().Mode.IsDeep() {
			// Debian needs aptitude to get changelogs.
			// Because unable to get changelogs via `apt-get changelog` on Debian.
			deps = append(deps, dep{
				packName: "aptitude",
				required: true,
				logFunc:  o.log.Errorf,
			})
		}
	}

	for _, dep := range deps {
		cmd := fmt.Sprintf("%s %s", dpkgQuery, dep.packName)
		msg := fmt.Sprintf("%s is not installed", dep.packName)
		r := o.exec(cmd, noSudo)
		if !r.isSuccess() {
			if dep.additionalMsg != "" {
				msg += dep.additionalMsg
			}
			dep.logFunc(msg)
			if dep.required {
				return fmt.Errorf(msg)
			}
			continue
		}

		_, status, _, _, _, _ := o.parseScannedPackagesLine(r.Stdout)
		if status != "ii" {
			if dep.additionalMsg != "" {
				msg += dep.additionalMsg
			}
			dep.logFunc(msg)
			if dep.required {
				return fmt.Errorf(msg)
			}
		}

	}
	o.log.Infof("Dependencies... Pass")
	return nil
}

func (o *debian) preCure() error {
	o.log.Infof("Scanning in %s", o.getServerInfo().Mode)
	if err := o.detectIPAddr(); err != nil {
		o.log.Debugf("Failed to detect IP addresses: %s", err)
	}
	// Ignore this error as it just failed to detect the IP addresses
	return nil
}

func (o *debian) postScan() error {
	if o.getServerInfo().Mode.IsDeep() || o.getServerInfo().Mode.IsFastRoot() {
		return o.checkrestart()
	}
	return nil
}

func (o *debian) detectIPAddr() (err error) {
	o.ServerInfo.IPv4Addrs, o.ServerInfo.IPv6Addrs, err = o.ip()
	return err
}

func (o *debian) scanPackages() error {
	// collect the running kernel information
	release, version, err := o.runningKernel()
	if err != nil {
		o.log.Errorf("Failed to scan the running kernel version: %s", err)
		return err
	}
	rebootRequired, err := o.rebootRequired()
	if err != nil {
		o.log.Errorf("Failed to detect the kernel reboot required: %s", err)
		return err
	}
	o.Kernel = models.Kernel{
		Version:        version,
		Release:        release,
		RebootRequired: rebootRequired,
	}

	installed, updatable, srcPacks, err := o.scanInstalledPackages()
	if err != nil {
		o.log.Errorf("Failed to scan installed packages: %s", err)
		return err
	}
	o.Packages = installed
	o.SrcPackages = srcPacks

	if o.getServerInfo().Mode.IsOffline() {
		return nil
	}

	if o.getServerInfo().Mode.IsDeep() || o.Distro.Family == config.Raspbian {
		unsecures, err := o.scanUnsecurePackages(updatable)
		if err != nil {
			o.log.Errorf("Failed to scan vulnerable packages: %s", err)
			return err
		}
		o.VulnInfos = unsecures
		return nil
	}
	return nil
}

// https://askubuntu.com/a/742844
func (o *debian) rebootRequired() (bool, error) {
	r := o.exec("test -f /var/run/reboot-required", noSudo)
	switch r.ExitStatus {
	case 0:
		return true, nil
	case 1:
		return false, nil
	default:
		return false, fmt.Errorf("Failed to check reboot reauired: %s", r)
	}
}

const dpkgQuery = `dpkg-query -W -f="\${binary:Package},\${db:Status-Abbrev},\${Version},\${Source},\${source:Version}\n"`

func (o *debian) scanInstalledPackages() (models.Packages, models.Packages, models.SrcPackages, error) {
	updatable := models.Packages{}
	r := o.exec(dpkgQuery, noSudo)
	if !r.isSuccess() {
		return nil, nil, nil, fmt.Errorf("Failed to SSH: %s", r)
	}

	installed, srcPacks, err := o.parseInstalledPackages(r.Stdout)
	if err != nil {
		return nil, nil, nil, err
	}

	if o.getServerInfo().Mode.IsOffline() || o.getServerInfo().Mode.IsFast() {
		return installed, updatable, srcPacks, nil
	}

	if err := o.aptGetUpdate(); err != nil {
		return nil, nil, nil, err
	}
	updatableNames, err := o.getUpdatablePackNames()
	if err != nil {
		return nil, nil, nil, err
	}
	for _, name := range updatableNames {
		for _, pack := range installed {
			if pack.Name == name {
				updatable[name] = pack
				break
			}
		}
	}

	// Fill the candidate versions of upgradable packages
	err = o.fillCandidateVersion(updatable)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("Failed to fill candidate versions. err: %s", err)
	}
	installed.MergeNewVersion(updatable)

	return installed, updatable, srcPacks, nil
}

func (o *debian) parseInstalledPackages(stdout string) (models.Packages, models.SrcPackages, error) {
	installed, srcPacks := models.Packages{}, models.SrcPackages{}

	// e.g.
	// curl,ii ,7.38.0-4+deb8u2,,7.38.0-4+deb8u2
	// openssh-server,ii ,1:6.7p1-5+deb8u3,openssh,1:6.7p1-5+deb8u3
	// tar,ii ,1.27.1-2+b1,tar (1.27.1-2),1.27.1-2
	lines := strings.Split(stdout, "\n")
	for _, line := range lines {
		if trimmed := strings.TrimSpace(line); len(trimmed) != 0 {
			name, status, version, srcName, srcVersion, err := o.parseScannedPackagesLine(trimmed)
			if err != nil || len(status) < 2 {
				return nil, nil, fmt.Errorf(
					"Debian: Failed to parse package line: %s", line)
			}

			packageStatus := status[1]
			// Package status:
			//     n = Not-installed
			//     c = Config-files
			//     H = Half-installed
			//     U = Unpacked
			//     F = Half-configured
			//     W = Triggers-awaiting
			//     t = Triggers-pending
			//     i = Installed
			if packageStatus != 'i' {
				o.log.Debugf("%s package status is '%c', ignoring", name, packageStatus)
				continue
			}
			installed[name] = models.Package{
				Name:    name,
				Version: version,
			}

			if srcName != "" && srcName != name {
				if pack, ok := srcPacks[srcName]; ok {
					pack.AddBinaryName(name)
					srcPacks[srcName] = pack
				} else {
					srcPacks[srcName] = models.SrcPackage{
						Name:        srcName,
						Version:     srcVersion,
						BinaryNames: []string{name},
					}
				}
			}
		}
	}

	// Remove "linux"
	// kernel-related packages are showed "linux" as source package name
	// If "linux" is left, oval detection will cause trouble, so delete.
	delete(srcPacks, "linux")
	// Remove duplicate
	for name := range installed {
		delete(srcPacks, name)
	}
	return installed, srcPacks, nil
}

func (o *debian) parseScannedPackagesLine(line string) (name, status, version, srcName, srcVersion string, err error) {
	ss := strings.Split(line, ",")
	if len(ss) == 5 {
		// remove :amd64, i386...
		name = ss[0]
		if i := strings.IndexRune(name, ':'); i >= 0 {
			name = name[:i]
		}
		status = strings.TrimSpace(ss[1])
		version = ss[2]
		// remove version. ex: tar (1.27.1-2)
		srcName = strings.Split(ss[3], " ")[0]
		srcVersion = ss[4]
		return
	}

	return "", "", "", "", "", fmt.Errorf("Unknown format: %s", line)
}

func (o *debian) aptGetUpdate() error {
	o.log.Infof("apt-get update...")
	cmd := util.PrependProxyEnv("apt-get update")
	if r := o.exec(cmd, sudo); !r.isSuccess() {
		return fmt.Errorf("Failed to apt-get update: %s", r)
	}
	return nil
}

func (o *debian) scanUnsecurePackages(updatable models.Packages) (models.VulnInfos, error) {
	// Setup changelog cache
	current := cache.Meta{
		Name:   o.getServerInfo().GetServerName(),
		Distro: o.getServerInfo().Distro,
		Packs:  updatable,
	}

	o.log.Debugf("Ensure changelog cache: %s", current.Name)
	meta, err := o.ensureChangelogCache(current)
	if err != nil {
		return nil, err
	}

	// Collect CVE information of upgradable packages
	vulnInfos, err := o.scanChangelogs(updatable, meta)
	if err != nil {
		return nil, fmt.Errorf("Failed to scan unsecure packages. err: %s", err)
	}

	return vulnInfos, nil
}

func (o *debian) ensureChangelogCache(current cache.Meta) (*cache.Meta, error) {
	// Search from cache
	cached, found, err := cache.DB.GetMeta(current.Name)
	if err != nil {
		return nil, fmt.Errorf(
			"Failed to get meta. Please remove cache.db and then try again. err: %s", err)
	}

	if !found {
		o.log.Debugf("Not found in meta: %s", current.Name)
		err = cache.DB.EnsureBuckets(current)
		if err != nil {
			return nil, fmt.Errorf("Failed to ensure buckets. err: %s", err)
		}
		return &current, nil
	}

	if current.Distro.Family != cached.Distro.Family ||
		current.Distro.Release != cached.Distro.Release {
		o.log.Debugf("Need to refesh meta: %s", current.Name)
		err = cache.DB.EnsureBuckets(current)
		if err != nil {
			return nil, fmt.Errorf("Failed to ensure buckets. err: %s", err)
		}
		return &current, nil

	}

	o.log.Debugf("Reuse meta: %s", current.Name)
	if config.Conf.Debug {
		cache.DB.PrettyPrint(current)
	}
	return &cached, nil
}

func (o *debian) fillCandidateVersion(updatables models.Packages) (err error) {
	names := []string{}
	for name := range updatables {
		names = append(names, name)
	}
	cmd := fmt.Sprintf("LANGUAGE=en_US.UTF-8 apt-cache policy %s", strings.Join(names, " "))
	r := o.exec(cmd, noSudo)
	if !r.isSuccess() {
		return fmt.Errorf("Failed to SSH: %s", r)
	}
	packAptPolicy := o.splitAptCachePolicy(r.Stdout)
	for k, v := range packAptPolicy {
		ver, err := o.parseAptCachePolicy(v, k)
		if err != nil {
			return fmt.Errorf("Failed to parse %s", err)
		}
		pack, ok := updatables[k]
		if !ok {
			return fmt.Errorf("Not found: %s", k)
		}
		pack.NewVersion = ver.Candidate
		pack.Repository = ver.Repo
		updatables[k] = pack
	}
	return
}

func (o *debian) getUpdatablePackNames() (packNames []string, err error) {
	cmd := util.PrependProxyEnv("LANGUAGE=en_US.UTF-8 apt-get dist-upgrade --dry-run")
	r := o.exec(cmd, noSudo)
	if r.isSuccess(0, 1) {
		return o.parseAptGetUpgrade(r.Stdout)
	}
	return packNames, fmt.Errorf(
		"Failed to %s. status: %d, stdout: %s, stderr: %s",
		cmd, r.ExitStatus, r.Stdout, r.Stderr)
}

func (o *debian) parseAptGetUpgrade(stdout string) (updatableNames []string, err error) {
	startRe := regexp.MustCompile(`The following packages will be upgraded:`)
	stopRe := regexp.MustCompile(`^(\d+) upgraded.*`)
	startLineFound, stopLineFound := false, false

	lines := strings.Split(stdout, "\n")
	for _, line := range lines {
		if !startLineFound {
			if matche := startRe.MatchString(line); matche {
				startLineFound = true
			}
			continue
		}
		result := stopRe.FindStringSubmatch(line)
		if len(result) == 2 {
			nUpdatable, err := strconv.Atoi(result[1])
			if err != nil {
				return nil, fmt.Errorf(
					"Failed to scan upgradable packages number. line: %s", line)
			}
			if nUpdatable != len(updatableNames) {
				return nil, fmt.Errorf(
					"Failed to scan upgradable packages, expected: %s, detected: %d",
					result[1], len(updatableNames))
			}
			stopLineFound = true
			o.log.Debugf("Found the stop line. line: %s", line)
			break
		}
		updatableNames = append(updatableNames, strings.Fields(line)...)
	}
	if !startLineFound {
		// no upgrades
		return
	}
	if !stopLineFound {
		// There are upgrades, but not found the stop line.
		return nil, fmt.Errorf("Failed to scan upgradable packages")
	}
	return
}

// DetectedCveID has CveID, Confidence and DetectionMethod fields
// LenientMatching will be true if this vulnerability is not detected by accurate version matching.
// see https://github.com/future-architect/vuls/pull/328
type DetectedCveID struct {
	CveID      string
	Confidence models.Confidence
}

func (o *debian) scanChangelogs(updatablePacks models.Packages, meta *cache.Meta) (models.VulnInfos, error) {
	type response struct {
		pack           *models.Package
		DetectedCveIDs []DetectedCveID
	}
	resChan := make(chan response, len(updatablePacks))
	errChan := make(chan error, len(updatablePacks))
	reqChan := make(chan models.Package, len(updatablePacks))
	defer close(resChan)
	defer close(errChan)
	defer close(reqChan)

	go func() {
		for _, pack := range updatablePacks {
			reqChan <- pack
		}
	}()

	timeout := time.After(30 * 60 * time.Second)
	concurrency := 10
	tasks := util.GenWorkers(concurrency)
	for range updatablePacks {
		tasks <- func() {
			select {
			case pack := <-reqChan:
				func(p models.Package) {
					changelog := o.getChangelogCache(meta, p)
					if 0 < len(changelog) {
						cveIDs, pack := o.getCveIDsFromChangelog(changelog, p.Name, p.Version)
						resChan <- response{pack, cveIDs}
						return
					}

					// if the changelog is not in cache or failed to get from local cache,
					// get the changelog of the package via internet.
					// After that, store it in the cache.
					if cveIDs, pack, err := o.fetchParseChangelog(p); err != nil {
						errChan <- err
					} else {
						resChan <- response{pack, cveIDs}
					}
				}(pack)
			}
		}
	}

	// { DetectedCveID{} : [package] }
	cvePackages := make(map[DetectedCveID][]string)
	errs := []error{}
	for i := 0; i < len(updatablePacks); i++ {
		select {
		case response := <-resChan:
			if response.pack == nil {
				continue
			}
			o.Packages[response.pack.Name] = *response.pack
			cves := response.DetectedCveIDs
			for _, cve := range cves {
				packNames, ok := cvePackages[cve]
				if ok {
					packNames = append(packNames, response.pack.Name)
				} else {
					packNames = []string{response.pack.Name}
				}
				cvePackages[cve] = packNames
			}
			o.log.Infof("(%d/%d) Scanned %s: %s",
				i+1, len(updatablePacks), response.pack.Name, cves)
		case err := <-errChan:
			errs = append(errs, err)
		case <-timeout:
			errs = append(errs, fmt.Errorf("Timeout scanPackageCveIDs"))
		}
	}
	if 0 < len(errs) {
		return nil, fmt.Errorf("%v", errs)
	}

	var cveIDs []DetectedCveID
	for k := range cvePackages {
		cveIDs = append(cveIDs, k)
	}
	o.log.Debugf("%d Cves are found. cves: %v", len(cveIDs), cveIDs)
	vinfos := models.VulnInfos{}
	for cveID, names := range cvePackages {
		affected := models.PackageStatuses{}
		for _, n := range names {
			affected = append(affected, models.PackageStatus{Name: n})
		}

		vinfos[cveID.CveID] = models.VulnInfo{
			CveID:            cveID.CveID,
			Confidences:      models.Confidences{cveID.Confidence},
			AffectedPackages: affected,
		}
	}

	// Update meta package information of changelog cache to the latest one.
	meta.Packs = updatablePacks
	if err := cache.DB.RefreshMeta(*meta); err != nil {
		return nil, err
	}

	return vinfos, nil
}

func (o *debian) getChangelogCache(meta *cache.Meta, pack models.Package) string {
	cachedPack, found := meta.Packs[pack.Name]
	if !found {
		o.log.Debugf("Not found in cache: %s", pack.Name)
		return ""
	}

	if cachedPack.NewVersion != pack.NewVersion {
		o.log.Debugf("Expired: %s, cache: %s, new: %s",
			pack.Name, cachedPack.NewVersion, pack.NewVersion)
		return ""
	}
	changelog, err := cache.DB.GetChangelog(meta.Name, pack.Name)
	if err != nil {
		o.log.Warnf("Failed to get changelog. bucket: %s, key:%s, err: %s",
			meta.Name, pack.Name, err)
		return ""
	}
	if len(changelog) == 0 {
		o.log.Debugf("Empty string: %s", pack.Name)
		return ""
	}

	o.log.Debugf("Hit: %s, %s, cache: %s, new: %s len: %d, %s...",
		meta.Name, pack.Name, cachedPack.NewVersion, pack.NewVersion, len(changelog), util.Truncate(changelog, 30))
	return changelog
}

func (o *debian) fetchParseChangelog(pack models.Package) ([]DetectedCveID, *models.Package, error) {
	cmd := ""
	switch o.Distro.Family {
	case config.Ubuntu, config.Raspbian:
		cmd = fmt.Sprintf(`PAGER=cat apt-get -q=2 changelog %s`, pack.Name)
	case config.Debian:
		cmd = fmt.Sprintf(`PAGER=cat aptitude -q=2 changelog %s`, pack.Name)
	}
	cmd = util.PrependProxyEnv(cmd)

	r := o.exec(cmd, noSudo)
	if !r.isSuccess() {
		o.log.Warnf("Failed to SSH: %s", r)
		// Ignore this Error.
		return nil, nil, nil
	}

	stdout := strings.Replace(r.Stdout, "\r", "", -1)
	cveIDs, clogFilledPack := o.getCveIDsFromChangelog(stdout, pack.Name, pack.Version)

	if clogFilledPack.Changelog.Method != models.FailedToGetChangelog {
		err := cache.DB.PutChangelog(
			o.getServerInfo().GetServerName(), pack.Name, pack.Changelog.Contents)
		if err != nil {
			return nil, nil, fmt.Errorf("Failed to put changelog into cache")
		}
	}

	// No error will be returned. Only logging.
	return cveIDs, clogFilledPack, nil
}

func (o *debian) getCveIDsFromChangelog(
	changelog, name, ver string) ([]DetectedCveID, *models.Package) {

	if cveIDs, pack, err := o.parseChangelog(
		changelog, name, ver, models.ChangelogExactMatch); err == nil {
		return cveIDs, pack
	}

	var verAfterColon string

	splittedByColon := strings.Split(ver, ":")
	if 1 < len(splittedByColon) {
		verAfterColon = splittedByColon[1]
		if cveIDs, pack, err := o.parseChangelog(
			changelog, name, verAfterColon, models.ChangelogLenientMatch); err == nil {
			return cveIDs, pack
		}
	}

	delim := []string{"+", "~", "build"}
	switch o.Distro.Family {
	case config.Ubuntu:
		delim = append(delim, config.Ubuntu)
	case config.Debian:
	case config.Raspbian:
	}

	for _, d := range delim {
		ss := strings.Split(ver, d)
		if 1 < len(ss) {
			if cveIDs, pack, err := o.parseChangelog(
				changelog, name, ss[0], models.ChangelogLenientMatch); err == nil {
				return cveIDs, pack
			}
		}

		ss = strings.Split(verAfterColon, d)
		if 1 < len(ss) {
			if cveIDs, pack, err := o.parseChangelog(
				changelog, name, ss[0], models.ChangelogLenientMatch); err == nil {
				return cveIDs, pack
			}
		}
	}

	// Only logging the error.
	o.log.Warnf("Failed to find the version in changelog: %s-%s", name, ver)
	o.log.Debugf("Changelog of %s-%s: %s", name, ver, changelog)

	// If the version is not in changelog, return entire changelog to put into cache
	pack := o.Packages[name]
	pack.Changelog = models.Changelog{
		Contents: changelog,
		Method:   models.FailedToFindVersionInChangelog,
	}

	return []DetectedCveID{}, &pack
}

var cveRe = regexp.MustCompile(`(CVE-\d{4}-\d{4,})`)

// Collect CVE-IDs included in the changelog.
// The version specified in argument(versionOrLater) is used to compare.
func (o *debian) parseChangelog(changelog, name, ver string, confidence models.Confidence) ([]DetectedCveID, *models.Package, error) {
	installedVer, err := version.NewVersion(ver)
	if err != nil {
		return nil, nil, fmt.Errorf("Failed to parse installed version: %s, %s", ver, err)
	}
	buf, cveIDs := []string{}, []string{}
	scanner := bufio.NewScanner(strings.NewReader(changelog))
	found := false
	for scanner.Scan() {
		line := scanner.Text()
		buf = append(buf, line)
		if matches := cveRe.FindAllString(line, -1); 0 < len(matches) {
			for _, m := range matches {
				cveIDs = util.AppendIfMissing(cveIDs, m)
			}
		}

		ss := strings.Fields(line)
		if len(ss) < 2 {
			continue
		}

		if !strings.HasPrefix(ss[1], "(") || !strings.HasSuffix(ss[1], ")") {
			continue
		}
		clogVer, err := version.NewVersion(ss[1][1 : len(ss[1])-1])
		if err != nil {
			continue
		}
		if installedVer.Equal(clogVer) || installedVer.GreaterThan(clogVer) {
			found = true
			break
		}
	}

	if !found {
		pack := o.Packages[name]
		pack.Changelog = models.Changelog{
			Contents: "",
			Method:   models.FailedToFindVersionInChangelog,
		}
		return nil, &pack, fmt.Errorf(
			"Failed to scan CVE IDs. The version is not in changelog. name: %s, version: %s",
			name, ver)
	}

	clog := models.Changelog{
		Contents: strings.Join(buf[0:len(buf)-1], "\n"),
		Method:   confidence.DetectionMethod,
	}
	pack := o.Packages[name]
	pack.Changelog = clog

	cves := []DetectedCveID{}
	for _, id := range cveIDs {
		cves = append(cves, DetectedCveID{id, confidence})
	}

	return cves, &pack, nil
}

func (o *debian) splitAptCachePolicy(stdout string) map[string]string {
	re := regexp.MustCompile(`(?m:^[^ \t]+:\r?\n)`)
	ii := re.FindAllStringIndex(stdout, -1)
	ri := []int{}
	for i := len(ii) - 1; 0 <= i; i-- {
		ri = append(ri, ii[i][0])
	}
	splitted := []string{}
	lasti := len(stdout)
	for _, i := range ri {
		splitted = append(splitted, stdout[i:lasti])
		lasti = i
	}

	packAptPolicy := map[string]string{}
	for _, r := range splitted {
		packName := r[:strings.Index(r, ":")]
		packAptPolicy[packName] = r
	}
	return packAptPolicy
}

type packCandidateVer struct {
	Name      string
	Installed string
	Candidate string
	Repo      string
}

// parseAptCachePolicy the stdout of parse pat-get cache policy
func (o *debian) parseAptCachePolicy(stdout, name string) (packCandidateVer, error) {
	ver := packCandidateVer{Name: name}
	lines := strings.Split(stdout, "\n")
	isRepoline := false
	for _, line := range lines {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		switch fields[0] {
		case "Installed:":
			ver.Installed = fields[1]
		case "Candidate:":
			ver.Candidate = fields[1]
			goto nextline
		default:
			// nop
		}
		if ver.Candidate != "" && strings.Contains(line, ver.Candidate) {
			isRepoline = true
			goto nextline
		}

		if isRepoline {
			ss := strings.Split(strings.TrimSpace(line), " ")
			if len(ss) == 5 {
				ver.Repo = ss[2]
				return ver, nil
			}
		}
	nextline:
	}
	return ver, fmt.Errorf("Unknown Format: %s", stdout)
}

func (o *debian) checkrestart() error {
	initName, err := o.detectInitSystem()
	if err != nil {
		o.log.Warn(err)
		// continue scanning
	}

	cmd := "LANGUAGE=en_US.UTF-8 checkrestart"
	r := o.exec(cmd, sudo)
	if !r.isSuccess() {
		return fmt.Errorf(
			"Failed to %s. status: %d, stdout: %s, stderr: %s",
			cmd, r.ExitStatus, r.Stdout, r.Stderr)
	}
	packs, unknownServices := o.parseCheckRestart(r.Stdout)
	pidService := map[string]string{}
	if initName == upstart {
		for _, s := range unknownServices {
			cmd := "LANGUAGE=en_US.UTF-8 initctl status " + s
			r := o.exec(cmd, sudo)
			if !r.isSuccess() {
				continue
			}
			if ss := strings.Fields(r.Stdout); len(ss) == 4 && ss[2] == "process" {
				pidService[ss[3]] = s
			}
		}
	}

	for i, p := range packs {
		pack := o.Packages[p.Name]
		pack.NeedRestartProcs = p.NeedRestartProcs
		o.Packages[p.Name] = pack

		for j, proc := range p.NeedRestartProcs {
			if proc.HasInit == false {
				continue
			}
			packs[i].NeedRestartProcs[j].InitSystem = initName
			if initName == systemd {
				name, err := o.detectServiceName(proc.PID)
				if err != nil {
					o.log.Warn(err)
					// continue scanning
				}
				packs[i].NeedRestartProcs[j].ServiceName = name
			} else {
				if proc.ServiceName == "" {
					if ss := strings.Fields(r.Stdout); len(ss) == 4 && ss[2] == "process" {
						if name, ok := pidService[ss[3]]; ok {
							packs[i].NeedRestartProcs[j].ServiceName = name
						}
					}
				}
			}
		}
		o.Packages[p.Name] = p
	}
	return nil
}

func (o *debian) parseCheckRestart(stdout string) (models.Packages, []string) {
	services := []string{}
	scanner := bufio.NewScanner(strings.NewReader(stdout))
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "service") && strings.HasSuffix(line, "restart") {
			ss := strings.Fields(line)
			if len(ss) != 3 {
				continue
			}
			services = append(services, ss[1])
		}
	}

	packs := models.Packages{}
	packName := ""
	hasInit := true
	scanner = bufio.NewScanner(strings.NewReader(stdout))
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasSuffix(line, "do not seem to have an associated init script to restart them:") {
			hasInit = false
			continue
		}
		if strings.HasSuffix(line, ":") && len(strings.Fields(line)) == 1 {
			packName = strings.TrimSuffix(line, ":")
			continue
		}
		if strings.HasPrefix(line, "\t") {
			ss := strings.Fields(line)
			if len(ss) != 2 {
				continue
			}

			serviceName := ""
			for _, s := range services {
				if packName == s {
					serviceName = s
				}
			}
			if p, ok := packs[packName]; ok {
				p.NeedRestartProcs = append(p.NeedRestartProcs, models.NeedRestartProcess{
					PID:         ss[0],
					Path:        ss[1],
					ServiceName: serviceName,
					HasInit:     hasInit,
				})
				packs[packName] = p
			} else {
				packs[packName] = models.Package{
					Name: packName,
					NeedRestartProcs: []models.NeedRestartProcess{
						{
							PID:         ss[0],
							Path:        ss[1],
							ServiceName: serviceName,
							HasInit:     hasInit,
						},
					},
				}
			}
		}
	}

	unknownServices := []string{}
	for _, s := range services {
		found := false
		for _, p := range packs {
			for _, proc := range p.NeedRestartProcs {
				if proc.ServiceName == s {
					found = true
				}
			}
		}
		if !found {
			unknownServices = append(unknownServices, s)
		}
	}
	return packs, unknownServices
}
