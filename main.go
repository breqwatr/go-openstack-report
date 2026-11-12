package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/xuri/excelize/v2"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack"
	"github.com/gophercloud/gophercloud/v2/openstack/blockstorage/v3/snapshots"
	"github.com/gophercloud/gophercloud/v2/openstack/blockstorage/v3/volumes"
	"github.com/gophercloud/gophercloud/v2/openstack/compute/v2/flavors"
	"github.com/gophercloud/gophercloud/v2/openstack/compute/v2/servers"
	"github.com/gophercloud/gophercloud/v2/openstack/identity/v3/tokens"
	"github.com/gophercloud/gophercloud/v2/openstack/image/v2/images"
	"github.com/gophercloud/gophercloud/v2/openstack/networking/v2/extensions/layer3/floatingips"
	"github.com/gophercloud/gophercloud/v2/openstack/networking/v2/ports"
)

const (
	toolName    = "reportctl"
	toolVersion = "0.3.0"
)

// scope is the single project this report covers. Every collection is pinned to
// it: the tool must never hand a customer another tenant's objects.
type scope struct {
	ProjectID   string
	ProjectName string
}

// resolveScope reads the project out of the token itself rather than trusting
// OS_PROJECT_ID, which callers routinely set to a project name.
func resolveScope(provider *gophercloud.ProviderClient) (scope, error) {
	res, ok := provider.GetAuthResult().(tokens.CreateResult)
	if !ok {
		return scope{}, fmt.Errorf("unexpected auth result %T", provider.GetAuthResult())
	}
	p, err := res.ExtractProject()
	if err != nil {
		return scope{}, err
	}
	if p == nil || p.ID == "" {
		return scope{}, fmt.Errorf("token is not project-scoped; set OS_PROJECT_NAME or OS_PROJECT_ID")
	}
	return scope{ProjectID: p.ID, ProjectName: p.Name}, nil
}

// collection records the outcome of one API sweep. vMetaData reports these so a
// partial failure is visible in the workbook rather than silently producing an
// understated inventory.
type collection struct {
	Service string
	Object  string
	Count   int
	Err     error
}

// inventory is the raw estate as collected, before any normalisation.
type inventory struct {
	Servers   []servers.Server
	Flavors   []flavors.Flavor
	Ports     []ports.Port
	Volumes   []volumes.Volume
	FIPs      []floatingips.FloatingIP
	Snapshots []snapshots.Snapshot
	Images    []images.Image

	Collections []collection
	AuthURL     string
	Generated   time.Time
	Scope       scope
}

func (inv *inventory) record(service, object string, count int, err error) {
	inv.Collections = append(inv.Collections, collection{service, object, count, err})
	if err != nil {
		log.Printf("WARNING: %s/%s collection failed: %v", service, object, err)
	}
}

func (inv *inventory) failed() int {
	n := 0
	for _, c := range inv.Collections {
		if c.Err != nil {
			n++
		}
	}
	return n
}

// sheetData is one tab: a header row plus rows of cell values.
type sheetData struct {
	Name    string
	Headers []string
	Rows    [][]any
}

func main() {
	ctx := context.Background()

	opts, err := openstack.AuthOptionsFromEnv()
	if err != nil {
		log.Fatalf("Failed to get auth options: %v", err)
	}

	if os.Getenv("OS_PROJECT_DOMAIN_ID") != "" {
		opts.DomainID = os.Getenv("OS_PROJECT_DOMAIN_ID")
	} else if v := os.Getenv("OS_PROJECT_DOMAIN_NAME"); v != "" {
		opts.DomainName = v
	}

	provider, err := openstack.AuthenticatedClient(ctx, opts)
	if err != nil {
		log.Fatalf("Failed to authenticate: %v", err)
	}

	sc, err := resolveScope(provider)
	if err != nil {
		log.Fatalf("Failed to determine project scope: %v", err)
	}
	log.Printf("Collecting inventory for project %s (%s)", sc.ProjectName, sc.ProjectID)

	inv := &inventory{AuthURL: opts.IdentityEndpoint, Generated: time.Now(), Scope: sc}
	collect(ctx, provider, inv)

	idx := buildIndexes(inv)

	sheets := []sheetData{
		buildVBilling(inv, idx),
		buildVInfo(inv, idx),
		buildVFlavor(inv, idx),
		buildVDisk(inv, idx),
		buildVNetwork(inv, idx),
		buildVSnapshot(inv, idx),
		buildVHealth(inv, idx),
		buildVMetaData(inv, idx),
	}

	filename := fmt.Sprintf("report-%s.xlsx", inv.Generated.Format("2006-01-02"))
	if err := writeWorkbook(filename, sheets); err != nil {
		log.Fatalf("Failed to write %s: %v", filename, err)
	}

	log.Printf("Wrote %s", filename)
	if n := inv.failed(); n > 0 {
		log.Printf("WARNING: %d collection(s) failed; see the vHealth and vMetaData tabs. Inventory is INCOMPLETE.", n)
		os.Exit(1)
	}
}

// collect gathers every object type, recording per-service success or failure
// rather than discarding errors.
func collect(ctx context.Context, provider *gophercloud.ProviderClient, inv *inventory) {
	compute, err := openstack.NewComputeV2(provider, gophercloud.EndpointOpts{})
	if err != nil {
		inv.record("compute", "endpoint", 0, err)
	} else {
		if pages, err := servers.List(compute, servers.ListOpts{}).AllPages(ctx); err != nil {
			inv.record("compute", "servers", 0, err)
		} else {
			inv.Servers, err = servers.ExtractServers(pages)
			inv.record("compute", "servers", len(inv.Servers), err)
		}

		if pages, err := flavors.ListDetail(compute, flavors.ListOpts{}).AllPages(ctx); err != nil {
			inv.record("compute", "flavors", 0, err)
		} else {
			inv.Flavors, err = flavors.ExtractFlavors(pages)
			inv.record("compute", "flavors", len(inv.Flavors), err)
		}
	}

	storage, err := openstack.NewBlockStorageV3(provider, gophercloud.EndpointOpts{})
	if err != nil {
		inv.record("blockstorage", "endpoint", 0, err)
	} else {
		if pages, err := volumes.List(storage, volumes.ListOpts{}).AllPages(ctx); err != nil {
			inv.record("blockstorage", "volumes", 0, err)
		} else {
			inv.Volumes, err = volumes.ExtractVolumes(pages)
			inv.record("blockstorage", "volumes", len(inv.Volumes), err)
		}

		if pages, err := snapshots.List(storage, snapshots.ListOpts{}).AllPages(ctx); err != nil {
			inv.record("blockstorage", "snapshots", 0, err)
		} else {
			inv.Snapshots, err = snapshots.ExtractSnapshots(pages)
			inv.record("blockstorage", "snapshots", len(inv.Snapshots), err)
		}
	}

	network, err := openstack.NewNetworkV2(provider, gophercloud.EndpointOpts{})
	if err != nil {
		inv.record("network", "endpoint", 0, err)
	} else {
		if pages, err := ports.List(network, ports.ListOpts{ProjectID: inv.Scope.ProjectID}).AllPages(ctx); err != nil {
			inv.record("network", "ports", 0, err)
		} else {
			inv.Ports, err = ports.ExtractPorts(pages)
			inv.record("network", "ports", len(inv.Ports), err)
		}

		if pages, err := floatingips.List(network, floatingips.ListOpts{ProjectID: inv.Scope.ProjectID}).AllPages(ctx); err != nil {
			inv.record("network", "floatingips", 0, err)
		} else {
			inv.FIPs, err = floatingips.ExtractFloatingIPs(pages)
			inv.record("network", "floatingips", len(inv.FIPs), err)
		}
	}

	// Images resolve the OS column on vBilling and the image name on vInfo. A
	// failure here degrades both to a raw image UUID, or to blank.
	if image, err := openstack.NewImageV2(provider, gophercloud.EndpointOpts{}); err != nil {
		inv.record("image", "endpoint", 0, err)
	} else if pages, err := images.List(image, images.ListOpts{}).AllPages(ctx); err != nil {
		inv.record("image", "images", 0, err)
	} else {
		inv.Images, err = images.ExtractImages(pages)
		inv.record("image", "images", len(inv.Images), err)
	}
}

// indexes holds the lookup maps, built once, so the tab builders stay linear
// instead of rescanning every slice per VM.
type indexes struct {
	flavorByID  map[string]flavors.Flavor
	imageByID   map[string]images.Image
	serverByID  map[string]servers.Server
	volumeByID  map[string]volumes.Volume
	portsByVM   map[string][]ports.Port
	portByID    map[string]ports.Port
	fipsByPort  map[string][]floatingips.FloatingIP
	volsByVM    map[string][]volumes.Volume
	deviceOfVol map[string]string // volumeID|serverID -> /dev/vdX
	flavorUsage map[string]int
}

func buildIndexes(inv *inventory) *indexes {
	idx := &indexes{
		flavorByID:  make(map[string]flavors.Flavor, len(inv.Flavors)),
		imageByID:   make(map[string]images.Image, len(inv.Images)),
		serverByID:  make(map[string]servers.Server, len(inv.Servers)),
		volumeByID:  make(map[string]volumes.Volume, len(inv.Volumes)),
		portsByVM:   make(map[string][]ports.Port),
		portByID:    make(map[string]ports.Port, len(inv.Ports)),
		fipsByPort:  make(map[string][]floatingips.FloatingIP),
		volsByVM:    make(map[string][]volumes.Volume),
		deviceOfVol: make(map[string]string),
		flavorUsage: make(map[string]int),
	}

	for _, f := range inv.Flavors {
		idx.flavorByID[f.ID] = f
	}
	for _, i := range inv.Images {
		idx.imageByID[i.ID] = i
	}
	for _, s := range inv.Servers {
		idx.serverByID[s.ID] = s
		if id, _ := flavorRef(s); id != "" {
			idx.flavorUsage[id]++
		}
	}
	for _, v := range inv.Volumes {
		idx.volumeByID[v.ID] = v
		seen := make(map[string]bool, len(v.Attachments))
		for _, a := range v.Attachments {
			if a.ServerID == "" || seen[a.ServerID] {
				continue // a multiattach volume can list the same server twice
			}
			seen[a.ServerID] = true
			idx.volsByVM[a.ServerID] = append(idx.volsByVM[a.ServerID], v)
			idx.deviceOfVol[v.ID+"|"+a.ServerID] = a.Device
		}
	}
	for _, p := range inv.Ports {
		idx.portByID[p.ID] = p
		if p.DeviceID != "" {
			idx.portsByVM[p.DeviceID] = append(idx.portsByVM[p.DeviceID], p)
		}
	}
	for _, f := range inv.FIPs {
		if f.PortID != "" {
			idx.fipsByPort[f.PortID] = append(idx.fipsByPort[f.PortID], f)
		}
	}
	return idx
}

// flavorRef safely pulls the flavor id and embedded name out of the server's
// flavor object. Microversion 2.47+ omits "id" entirely, so neither is assumed.
func flavorRef(s servers.Server) (id, name string) {
	if s.Flavor == nil {
		return "", ""
	}
	if v, ok := s.Flavor["id"].(string); ok {
		id = v
	}
	if v, ok := s.Flavor["original_name"].(string); ok {
		name = v
	}
	return id, name
}

func imageRef(s servers.Server) string {
	if s.Image == nil {
		return ""
	}
	if v, ok := s.Image["id"].(string); ok {
		return v
	}
	return ""
}

func (idx *indexes) fipsForVM(vmID string) []string {
	var out []string
	for _, p := range idx.portsByVM[vmID] {
		for _, f := range idx.fipsByPort[p.ID] {
			out = append(out, f.FloatingIP)
		}
	}
	sort.Strings(out)
	return out
}

func buildVInfo(inv *inventory, idx *indexes) sheetData {
	s := sheetData{Name: "vInfo", Headers: []string{
		"VM Name", "Hostname", "VM ID", "Status", "Power State", "Task State", "Project ID",
		"Availability Zone", "Flavor", "Flavor ID", "vCPUs", "RAM (MB)",
		"Root Disk (GB)", "Image", "Image ID", "Key Name", "License", "Boot Source",
		"Disks", "Disk Total (GB)", "NICs", "Floating IPs", "Locked", "Created",
		"Updated", "Fault", "Properties",
	}}

	for _, vm := range inv.Servers {
		flavorID, flavorName := flavorRef(vm)
		var vcpus, ram, rootDisk any
		if f, ok := idx.flavorByID[flavorID]; ok {
			flavorName, vcpus, ram, rootDisk = f.Name, f.VCPUs, f.RAM, f.Disk
		}

		imageID := imageRef(vm)
		imageName := imageID
		if img, ok := idx.imageByID[imageID]; ok {
			imageName = img.Name
		}

		vols := idx.volsByVM[vm.ID]
		diskTotal := 0
		for _, v := range vols {
			diskTotal += v.Size
		}

		locked := false
		if vm.Locked != nil {
			locked = *vm.Locked
		}

		s.Rows = append(s.Rows, []any{
			vm.Name, deref(vm.Hostname), vm.ID, vm.Status, vm.PowerState.String(), vm.TaskState, vm.TenantID,
			vm.AvailabilityZone, flavorName, flavorID, vcpus, ram,
			rootDisk, imageName, imageID, vm.KeyName, vm.Metadata["license_type"], bootSource(vm, vols),
			len(vols), diskTotal, len(idx.portsByVM[vm.ID]), strings.Join(idx.fipsForVM(vm.ID), ", "),
			yesNo(locked), ts(vm.Created), ts(vm.Updated), vm.Fault.Message, kvString(vm.Metadata),
		})
	}

	sortRows(s.Rows, 0, 1)
	return s
}

func buildVFlavor(inv *inventory, idx *indexes) sheetData {
	s := sheetData{Name: "vFlavor", Headers: []string{
		"Flavor Name", "Flavor ID", "vCPUs", "RAM (MB)", "Disk (GB)", "Ephemeral (GB)",
		"Swap (MB)", "RxTx Factor", "Public", "VMs Using", "Description", "Extra Specs",
	}}

	for _, f := range inv.Flavors {
		s.Rows = append(s.Rows, []any{
			f.Name, f.ID, f.VCPUs, f.RAM, f.Disk, f.Ephemeral,
			f.Swap, f.RxTxFactor, yesNo(f.IsPublic), idx.flavorUsage[f.ID], f.Description, kvString(f.ExtraSpecs),
		})
	}

	sortRows(s.Rows, 0, 1)
	return s
}

// buildVDisk emits one row per volume attachment, plus one row per unattached
// volume, replacing the old dynamic "Disk 1..N" columns.
func buildVDisk(inv *inventory, idx *indexes) sheetData {
	s := sheetData{Name: "vDisk", Headers: []string{
		"VM Name", "VM ID", "Device", "Volume Name", "Volume ID", "Size (GB)",
		"Volume Type", "Bootable", "Encrypted", "Multiattach", "Attached", "Status",
		"Availability Zone", "Project ID", "Created",
	}}

	row := func(vmName, vmID, device string, v volumes.Volume, attached bool) []any {
		return []any{
			vmName, vmID, device, v.Name, v.ID, v.Size,
			v.VolumeType, v.Bootable, yesNo(v.Encrypted), yesNo(v.Multiattach), yesNo(attached), v.Status,
			v.AvailabilityZone, v.TenantID, ts(v.CreatedAt),
		}
	}

	for _, v := range inv.Volumes {
		seen := make(map[string]bool, len(v.Attachments))
		attached := false
		for _, a := range v.Attachments {
			if a.ServerID == "" || seen[a.ServerID] {
				continue
			}
			seen[a.ServerID] = true
			attached = true
			name := ""
			if vm, ok := idx.serverByID[a.ServerID]; ok {
				name = vm.Name
			}
			s.Rows = append(s.Rows, row(name, a.ServerID, a.Device, v, true))
		}
		if !attached {
			s.Rows = append(s.Rows, row("", "", "", v, false))
		}
	}

	sortRows(s.Rows, 0, 2, 4)
	return s
}

func buildVNetwork(inv *inventory, idx *indexes) sheetData {
	s := sheetData{Name: "vNetwork", Headers: []string{
		"VM Name", "VM ID", "Port ID", "Port Name", "Status", "MAC Address",
		"Fixed IPs", "Floating IPs", "Network ID", "Device Owner", "Security Groups",
		"Admin State Up", "Project ID", "Created",
	}}

	for _, p := range inv.Ports {
		vmName := ""
		if vm, ok := idx.serverByID[p.DeviceID]; ok {
			vmName = vm.Name
		}
		var fips []string
		for _, f := range idx.fipsByPort[p.ID] {
			fips = append(fips, f.FloatingIP)
		}
		sort.Strings(fips)

		s.Rows = append(s.Rows, []any{
			vmName, p.DeviceID, p.ID, p.Name, p.Status, p.MACAddress,
			fixedIPs(p), strings.Join(fips, ", "), p.NetworkID, p.DeviceOwner, strings.Join(p.SecurityGroups, ", "),
			yesNo(p.AdminStateUp), p.ProjectID, ts(p.CreatedAt),
		})
	}

	sortRows(s.Rows, 0, 2)
	return s
}

func buildVSnapshot(inv *inventory, idx *indexes) sheetData {
	s := sheetData{Name: "vSnapshot", Headers: []string{
		"Snapshot Name", "Snapshot ID", "Status", "Size (GB)", "Volume Name",
		"Volume ID", "Progress", "Description", "Created",
	}}

	for _, sn := range inv.Snapshots {
		volName := ""
		if v, ok := idx.volumeByID[sn.VolumeID]; ok {
			volName = v.Name
		}
		s.Rows = append(s.Rows, []any{
			sn.Name, sn.ID, sn.Status, sn.Size, volName,
			sn.VolumeID, sn.Progress, sn.Description, ts(sn.CreatedAt),
		})
	}

	sortRows(s.Rows, 0, 1)
	return s
}

// buildVHealth is the derived tab: problems worth acting on, not raw inventory.
func buildVHealth(inv *inventory, idx *indexes) sheetData {
	s := sheetData{Name: "vHealth", Headers: []string{
		"Severity", "Category", "Object Type", "Object", "Object ID", "Detail",
	}}
	add := func(sev, cat, typ, name, id, detail string) {
		s.Rows = append(s.Rows, []any{sev, cat, typ, name, id, detail})
	}

	for _, c := range inv.Collections {
		if c.Err != nil {
			add("ERROR", "Collection", "Service", c.Service+"/"+c.Object, "",
				"Collection failed, inventory is incomplete: "+c.Err.Error())
		}
	}

	// Defence in depth: if any API ever returns an object outside the scoped
	// project, say so loudly rather than quietly shipping it to a customer.
	foreign := func(typ, name, id, project string) {
		if project != "" && project != inv.Scope.ProjectID {
			add("ERROR", "Scope", typ, name, id,
				"Object belongs to project "+project+", outside the reported scope "+inv.Scope.ProjectID)
		}
	}
	for _, vm := range inv.Servers {
		foreign("VM", vm.Name, vm.ID, vm.TenantID)
	}
	for _, v := range inv.Volumes {
		foreign("Volume", v.Name, v.ID, v.TenantID)
	}
	for _, sn := range inv.Snapshots {
		foreign("Snapshot", sn.Name, sn.ID, sn.ProjectID)
	}
	for _, p := range inv.Ports {
		foreign("Port", p.Name, p.ID, p.ProjectID)
	}
	for _, f := range inv.FIPs {
		foreign("Floating IP", f.FloatingIP, f.ID, f.ProjectID)
	}

	for _, vm := range inv.Servers {
		switch {
		case strings.EqualFold(vm.Status, "ERROR"):
			detail := vm.Fault.Message
			if detail == "" {
				detail = "VM is in ERROR state"
			}
			add("ERROR", "Compute", "VM", vm.Name, vm.ID, detail)
		case strings.EqualFold(vm.Status, "SHUTOFF"):
			add("INFO", "Compute", "VM", vm.Name, vm.ID, "VM is powered off but still consuming storage")
		}
		if id, name := flavorRef(vm); id != "" {
			if _, ok := idx.flavorByID[id]; !ok {
				ref := id
				if name != "" {
					ref = fmt.Sprintf("%s (%s)", name, id)
				}
				add("WARNING", "Compute", "VM", vm.Name, vm.ID,
					"References flavor "+ref+" which no longer exists")
			}
		}
		if len(idx.portsByVM[vm.ID]) == 0 {
			add("WARNING", "Network", "VM", vm.Name, vm.ID, "VM has no network ports")
		}
	}

	for _, v := range inv.Volumes {
		if strings.Contains(strings.ToLower(v.Status), "error") {
			add("ERROR", "Storage", "Volume", v.Name, v.ID, "Volume status is "+v.Status)
		}
		if len(v.Attachments) == 0 {
			add("WARNING", "Storage", "Volume", v.Name, v.ID,
				fmt.Sprintf("Unattached volume consuming %d GB", v.Size))
			continue
		}
		for _, a := range v.Attachments {
			if _, ok := idx.serverByID[a.ServerID]; !ok && a.ServerID != "" {
				add("ERROR", "Storage", "Volume", v.Name, v.ID,
					"Attached to server "+a.ServerID+" which was not found")
			}
		}
	}

	for _, sn := range inv.Snapshots {
		if strings.Contains(strings.ToLower(sn.Status), "error") {
			add("ERROR", "Storage", "Snapshot", sn.Name, sn.ID, "Snapshot status is "+sn.Status)
		}
		if _, ok := idx.volumeByID[sn.VolumeID]; !ok {
			add("WARNING", "Storage", "Snapshot", sn.Name, sn.ID,
				fmt.Sprintf("Orphaned, source volume %s not found, consuming %d GB", sn.VolumeID, sn.Size))
		}
	}

	for _, f := range inv.FIPs {
		if f.PortID == "" {
			add("WARNING", "Network", "Floating IP", f.FloatingIP, f.ID, "Allocated but not associated with any port")
		}
	}

	for _, p := range inv.Ports {
		if p.DeviceID == "" {
			add("WARNING", "Network", "Port", p.Name, p.ID, "Port has no device attached")
		}
	}

	rank := map[string]int{"ERROR": 0, "WARNING": 1, "INFO": 2}
	sort.SliceStable(s.Rows, func(i, j int) bool {
		a, b := s.Rows[i], s.Rows[j]
		if ra, rb := rank[a[0].(string)], rank[b[0].(string)]; ra != rb {
			return ra < rb
		}
		for _, c := range []int{1, 2, 3} {
			if a[c].(string) != b[c].(string) {
				return a[c].(string) < b[c].(string)
			}
		}
		return false
	})
	return s
}

func buildVMetaData(inv *inventory, idx *indexes) sheetData {
	s := sheetData{Name: "vMetaData", Headers: []string{"Section", "Item", "Value"}}
	add := func(section, item string, value any) {
		s.Rows = append(s.Rows, []any{section, item, value})
	}

	add("Report", "Generated", ts(inv.Generated))
	add("Report", "Tool", toolName)
	add("Report", "Version", toolVersion)
	add("Report", "Auth URL", inv.AuthURL)
	add("Report", "Project", inv.Scope.ProjectName)
	add("Report", "Project ID", inv.Scope.ProjectID)
	add("Report", "Scope", "single project")

	for _, c := range inv.Collections {
		status := fmt.Sprintf("OK, %d collected", c.Count)
		if c.Err != nil {
			status = "FAILED: " + c.Err.Error()
		}
		add("Collection", c.Service+"/"+c.Object, status)
	}

	var vcpus, ram, attached, unattached, snapSize, unassociated int
	for _, vm := range inv.Servers {
		if id, _ := flavorRef(vm); id != "" {
			if f, ok := idx.flavorByID[id]; ok {
				vcpus += f.VCPUs
				ram += f.RAM
			}
		}
	}
	for _, v := range inv.Volumes {
		if len(v.Attachments) == 0 {
			unattached += v.Size
		} else {
			attached += v.Size
		}
	}
	for _, sn := range inv.Snapshots {
		snapSize += sn.Size
	}
	for _, f := range inv.FIPs {
		if f.PortID == "" {
			unassociated++
		}
	}

	add("Totals", "VMs", len(inv.Servers))
	add("Totals", "vCPUs", vcpus)
	add("Totals", "RAM (GB)", ram/1024)
	add("Totals", "Flavors", len(inv.Flavors))
	add("Totals", "Volumes", len(inv.Volumes))
	add("Totals", "Attached Storage (GB)", attached)
	add("Totals", "Unattached Storage (GB)", unattached)
	add("Totals", "Snapshots", len(inv.Snapshots))
	add("Totals", "Snapshot Size (GB)", snapSize)
	add("Totals", "Ports", len(inv.Ports))
	add("Totals", "Floating IPs", len(inv.FIPs))
	add("Totals", "Unassociated Floating IPs", unassociated)

	return s
}

func writeWorkbook(filename string, sheets []sheetData) error {
	f := excelize.NewFile()
	defer f.Close()

	headerStyle, err := f.NewStyle(&excelize.Style{
		Font: &excelize.Font{Bold: true},
		Fill: excelize.Fill{Type: "pattern", Pattern: 1, Color: []string{"#DDEBF7"}},
	})
	if err != nil {
		return err
	}

	for _, s := range sheets {
		if err := writeSheet(f, s, headerStyle); err != nil {
			return fmt.Errorf("sheet %s: %w", s.Name, err)
		}
	}

	if err := f.DeleteSheet("Sheet1"); err != nil {
		return err
	}
	if idx, err := f.GetSheetIndex(sheets[0].Name); err == nil {
		f.SetActiveSheet(idx)
	}
	return f.SaveAs(filename)
}

func writeSheet(f *excelize.File, s sheetData, headerStyle int) error {
	if _, err := f.NewSheet(s.Name); err != nil {
		return err
	}
	if err := f.SetSheetRow(s.Name, "A1", &s.Headers); err != nil {
		return err
	}
	for i := range s.Rows {
		cell, err := excelize.CoordinatesToCellName(1, i+2)
		if err != nil {
			return err
		}
		if err := f.SetSheetRow(s.Name, cell, &s.Rows[i]); err != nil {
			return err
		}
	}

	lastCol, err := excelize.ColumnNumberToName(len(s.Headers))
	if err != nil {
		return err
	}
	if err := f.SetCellStyle(s.Name, "A1", lastCol+"1", headerStyle); err != nil {
		return err
	}
	if err := f.SetPanes(s.Name, &excelize.Panes{
		Freeze: true, YSplit: 1, TopLeftCell: "A2", ActivePane: "bottomLeft",
	}); err != nil {
		return err
	}
	if err := f.AutoFilter(s.Name, fmt.Sprintf("A1:%s%d", lastCol, len(s.Rows)+1), nil); err != nil {
		return err
	}

	// Size each column to its widest value, capped so a long fault message or
	// extra-specs blob does not push the rest of the sheet off screen.
	for c, h := range s.Headers {
		width := len(h)
		for _, row := range s.Rows {
			if c < len(row) {
				if n := len(fmt.Sprintf("%v", row[c])); n > width {
					width = n
				}
			}
		}
		name, err := excelize.ColumnNumberToName(c + 1)
		if err != nil {
			return err
		}
		if err := f.SetColWidth(s.Name, name, name, float64(min(width+2, 60))); err != nil {
			return err
		}
	}
	return nil
}

// sortRows orders rows by the given column indexes so successive runs of the
// tool produce diffable workbooks.
func sortRows(rows [][]any, cols ...int) {
	sort.SliceStable(rows, func(i, j int) bool {
		for _, c := range cols {
			a := fmt.Sprintf("%v", rows[i][c])
			b := fmt.Sprintf("%v", rows[j][c])
			if a != b {
				return a < b
			}
		}
		return false
	})
}

func fixedIPs(p ports.Port) string {
	out := make([]string, 0, len(p.FixedIPs))
	for _, ip := range p.FixedIPs {
		out = append(out, ip.IPAddress)
	}
	sort.Strings(out)
	return strings.Join(out, ", ")
}

func kvString(m map[string]string) string {
	if len(m) == 0 {
		return ""
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"="+m[k])
	}
	return strings.Join(parts, "; ")
}

func ts(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format("2006-01-02 15:04:05")
}

// deref renders an optional string field, several of which Nova returns as null
// when the caller lacks the policy for extended server attributes.
func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

// ---------------------------------------------------------------------------
// vBilling.
//
// One row per VM, disks spread across columns, in the RVTools wide idiom. A
// volume or address with no VM attached gets its own row, keyed "unassigned",
// which is the whole point: it is billed whether or not anything is using it,
// and a blank cell reads as missing data rather than as a charge.
// ---------------------------------------------------------------------------

// unassigned keys the rows for objects no VM is using. Named rather than blank
// so a reader scanning the VM column sees a thing they are paying for instead
// of skimming past an empty cell.
const unassigned = "unassigned"

// bootSource reports where a server's root disk lives. It decides whether the
// flavor's disk is real local storage or merely the shape of a root that is
// actually a Cinder volume listed among the disks in its own right.
func bootSource(vm servers.Server, vols []volumes.Volume) string {
	for _, v := range vols {
		if strings.EqualFold(v.Bootable, "true") {
			return "volume"
		}
	}
	if imageRef(vm) == "" {
		return "unknown"
	}
	return "image"
}

// vmDisks lists a VM's disks in GB, left to right. An image-booted VM leads
// with its flavor root plus ephemeral: that is real local storage which nothing
// else on this sheet would show. A volume-booted VM does not, because its root
// IS one of the volumes that follow and counting the flavor disk as well would
// bill it twice.
func vmDisks(vm servers.Server, idx *indexes) []int {
	vols := idx.volsByVM[vm.ID]
	var out []int

	id, _ := flavorRef(vm)
	if f, ok := idx.flavorByID[id]; ok && bootSource(vm, vols) != "volume" {
		if n := f.Disk + f.Ephemeral; n > 0 {
			out = append(out, n)
		}
	}

	// Order by device path so a VM's disks land in the same columns run to run.
	sorted := append([]volumes.Volume(nil), vols...)
	sort.Slice(sorted, func(i, j int) bool {
		di := idx.deviceOfVol[sorted[i].ID+"|"+vm.ID]
		dj := idx.deviceOfVol[sorted[j].ID+"|"+vm.ID]
		if (di == "") != (dj == "") {
			return dj == "" // a known device path comes before an unknown one
		}
		if di != dj {
			return di < dj
		}
		return sorted[i].ID < sorted[j].ID
	})
	for _, v := range sorted {
		out = append(out, v.Size)
	}
	return out
}

// osName is the Glance image the VM booted. Nova reports no guest OS, so the
// image is the closest thing there is. A volume-booted VM has no image of its
// own, so the name comes from the metadata Cinder copies onto the volume.
func osName(vm servers.Server, idx *indexes) string {
	if img, ok := idx.imageByID[imageRef(vm)]; ok {
		return img.Name
	}
	for _, v := range idx.volsByVM[vm.ID] {
		if !strings.EqualFold(v.Bootable, "true") {
			continue
		}
		if n := v.VolumeImageMetadata["image_name"]; n != "" {
			return n
		}
		if img, ok := idx.imageByID[v.VolumeImageMetadata["image_id"]]; ok {
			return img.Name
		}
	}
	return ""
}

func buildVBilling(inv *inventory, idx *indexes) sheetData {
	// rank keeps the VMs together at the top, then the disks with no VM, then
	// the addresses with no VM, rather than interleaving them on a blank name.
	type entry struct {
		rank   int
		vmName string
		flavor string
		os     string
		disks  []int
		fip    string
	}
	var entries []entry
	widest := 1

	for _, vm := range inv.Servers {
		_, flavorName := flavorRef(vm)
		if id, _ := flavorRef(vm); id != "" {
			if f, ok := idx.flavorByID[id]; ok {
				flavorName = f.Name
			}
		}
		e := entry{0, vm.Name, flavorName, osName(vm, idx), vmDisks(vm, idx), strings.Join(idx.fipsForVM(vm.ID), ", ")}
		entries = append(entries, e)
		widest = max(widest, len(e.disks))
	}

	// A volume nothing is attached to. Available is the common case, but a
	// volume stuck in error or reserved is holding capacity just the same.
	for _, v := range inv.Volumes {
		attached := false
		for _, a := range v.Attachments {
			if a.ServerID != "" {
				attached = true
				break
			}
		}
		if !attached {
			entries = append(entries, entry{rank: 1, vmName: unassigned, disks: []int{v.Size}})
		}
	}

	// Same rule applied to addresses: a floating IP held by the project but not
	// bound to a VM is still billed, and has no VM to sit behind either.
	for _, f := range inv.FIPs {
		if _, id := idx.vmForFIP(f); id == "" {
			entries = append(entries, entry{rank: 2, vmName: unassigned, fip: f.FloatingIP})
		}
	}

	sort.SliceStable(entries, func(i, j int) bool {
		a, b := entries[i], entries[j]
		if a.rank != b.rank {
			return a.rank < b.rank
		}
		if a.vmName != b.vmName {
			return a.vmName < b.vmName
		}
		return a.fip < b.fip
	})

	s := sheetData{Name: "vBilling", Headers: []string{"VM Name", "Flavor", "OS"}}
	for i := 1; i <= widest; i++ {
		s.Headers = append(s.Headers, fmt.Sprintf("Disk %d", i))
	}
	s.Headers = append(s.Headers, "Floating IP")

	for _, e := range entries {
		row := []any{e.vmName, e.flavor, e.os}
		for i := 0; i < widest; i++ {
			if i < len(e.disks) {
				row = append(row, e.disks[i]) // GB, so the disk columns sum
			} else {
				row = append(row, nil)
			}
		}
		s.Rows = append(s.Rows, append(row, e.fip))
	}
	return s
}

// vmForFIP resolves a floating IP to the VM behind its port. Neutron records
// only the port, so an unbound address, or one bound to a router or load
// balancer, resolves to no VM at all. That is not an error: it is exactly the
// case that has to get its own row instead of vanishing.
func (idx *indexes) vmForFIP(f floatingips.FloatingIP) (name, id string) {
	p, ok := idx.portByID[f.PortID]
	if !ok {
		return "", ""
	}
	if vm, ok := idx.serverByID[p.DeviceID]; ok {
		return vm.Name, vm.ID
	}
	return "", ""
}
