package main

import (
	"fmt"
	"testing"
	"time"

	"github.com/gophercloud/gophercloud/v2/openstack/blockstorage/v3/snapshots"
	"github.com/gophercloud/gophercloud/v2/openstack/blockstorage/v3/volumes"
	"github.com/gophercloud/gophercloud/v2/openstack/compute/v2/flavors"
	"github.com/gophercloud/gophercloud/v2/openstack/compute/v2/servers"
	"github.com/gophercloud/gophercloud/v2/openstack/image/v2/images"
	"github.com/gophercloud/gophercloud/v2/openstack/networking/v2/extensions/layer3/floatingips"
	"github.com/gophercloud/gophercloud/v2/openstack/networking/v2/ports"
)

func fixture() *inventory {
	const proj = "proj-1"
	inv := &inventory{
		AuthURL:   "https://keystone.example/v3",
		Generated: time.Date(2026, 8, 31, 9, 0, 0, 0, time.UTC),
		Scope:     scope{ProjectID: proj, ProjectName: "acme-prod"},
		Flavors: []flavors.Flavor{
			{ID: "f1", Name: "m1.large", VCPUs: 4, RAM: 8192, Disk: 40, Ephemeral: 10},
			{ID: "f2", Name: "m1.tiny", VCPUs: 1, RAM: 1536, Disk: 20},
		},
		Servers: []servers.Server{
			// image-booted: flavor disk is chargeable local storage
			{ID: "vm1", Name: "web-01", Status: "ACTIVE", TenantID: proj, Flavor: map[string]any{"id": "f1"}, Image: map[string]any{"id": "img1"}},
			// volume-booted: flavor disk must NOT be counted, the volume is
			{ID: "vm2", Name: "db-01", Status: "SHUTOFF", TenantID: proj, Flavor: map[string]any{"id": "f2"}},
			// deleted flavor: allocation is unknown, not zero
			{ID: "vm3", Name: "orphan-01", Status: "ERROR", TenantID: proj, Flavor: map[string]any{"id": "gone", "original_name": "m1.old"}, Image: map[string]any{"id": "img1"}},
		},
		Volumes: []volumes.Volume{
			{ID: "v1", Name: "web-data", Size: 100, Status: "in-use", TenantID: proj, VolumeType: "ssd",
				Attachments: []volumes.Attachment{{ServerID: "vm1", Device: "/dev/vdb"}}},
			// multiattach across two VMs: one row, charged once
			{ID: "v2", Name: "shared", Size: 200, Status: "in-use", TenantID: proj, Multiattach: true,
				Attachments: []volumes.Attachment{{ServerID: "vm1", Device: "/dev/vdc"}, {ServerID: "vm2", Device: "/dev/vdb"}, {ServerID: "vm1", Device: "/dev/vdc"}}},
			// volume-backed root for vm2
			{ID: "v3", Name: "db-root", Size: 50, Status: "in-use", Bootable: "true", TenantID: proj,
				VolumeImageMetadata: map[string]string{"image_name": "ubuntu-24.04"},
				Attachments:         []volumes.Attachment{{ServerID: "vm2", Device: "/dev/vda"}}},
			// the customer's case: no VM to sit behind
			{ID: "v4", Name: "left-over", Size: 150, Status: "available", TenantID: proj},
			{ID: "v5", Name: "broken", Size: 25, Status: "error", TenantID: proj},
		},
		Snapshots: []snapshots.Snapshot{
			{ID: "s1", Name: "nightly", Size: 100, Status: "available", VolumeID: "v1", ProjectID: proj, ConsumesQuota: true},
			{ID: "s2", Name: "orphaned", Size: 40, Status: "available", VolumeID: "gone", ProjectID: proj},
		},
		Ports: []ports.Port{
			{ID: "p1", DeviceID: "vm1", ProjectID: proj},
			{ID: "p2", DeviceID: "router-1", ProjectID: proj},
		},
		FIPs: []floatingips.FloatingIP{
			{ID: "fip1", FloatingIP: "203.0.113.10", PortID: "p1", Status: "ACTIVE", ProjectID: proj},
			{ID: "fip2", FloatingIP: "203.0.113.11", PortID: "p2", Status: "ACTIVE", ProjectID: proj},
			{ID: "fip3", FloatingIP: "203.0.113.12", Status: "DOWN", ProjectID: proj},
		},
		Images: []images.Image{
			{ID: "img1", Name: "ubuntu-24.04", Owner: proj, Status: "active", Visibility: "private", DiskFormat: "qcow2", SizeBytes: 3_000_000_000},
			{ID: "img2", Name: "vendor-public", Owner: "someone-else", Status: "active", Visibility: "public", SizeBytes: 9_000_000_000},
		},
	}
	inv.record("compute", "servers", 3, nil)
	inv.record("network", "loadbalancers", 0, fmt.Errorf("octavia endpoint not found"))
	return inv
}

// row finds a vBilling row by VM name; blank matches the first VM-less row.
func rows(s sheetData) map[string][]any {
	out := make(map[string][]any, len(s.Rows))
	for _, r := range s.Rows {
		out[fmt.Sprint(r[0])] = r
	}
	return out
}

func TestVBillingShape(t *testing.T) {
	inv := fixture()
	s := buildVBilling(inv, buildIndexes(inv))

	want := []string{"VM Name", "Flavor", "OS", "Disk 1", "Disk 2", "Disk 3", "Floating IP"}
	if fmt.Sprint(s.Headers) != fmt.Sprint(want) {
		t.Fatalf("headers = %v, want %v", s.Headers, want)
	}

	// 3 VMs, 2 volumes with no VM, 2 addresses with no VM (one unbound, one on
	// a router port).
	if len(s.Rows) != 7 {
		t.Fatalf("got %d rows, want 7", len(s.Rows))
	}
	for i, r := range s.Rows {
		if len(r) != len(s.Headers) {
			t.Errorf("row %d has %d cells, want %d", i, len(r), len(s.Headers))
		}
	}
}

func TestVBillingMapsDisksToTheirVM(t *testing.T) {
	inv := fixture()
	r := rows(buildVBilling(inv, buildIndexes(inv)))

	// Image-booted: flavor root 40 plus 10 ephemeral, then /dev/vdb, then /dev/vdc.
	if got := fmt.Sprint(r["web-01"]); got != "[web-01 m1.large ubuntu-24.04 50 100 200 203.0.113.10]" {
		t.Errorf("web-01 = %s", got)
	}
	// Volume-booted: no flavor disk, because /dev/vda IS the root volume.
	if got := fmt.Sprint(r["db-01"]); got != "[db-01 m1.tiny ubuntu-24.04 50 200 <nil> ]" {
		t.Errorf("db-01 = %s", got)
	}
	// An unresolvable flavor still names itself and leaves the disks blank
	// rather than reporting zero.
	if got := fmt.Sprint(r["orphan-01"]); got != "[orphan-01 m1.old ubuntu-24.04 <nil> <nil> <nil> ]" {
		t.Errorf("orphan-01 = %s", got)
	}
}

// The customer's case: a volume nothing is attached to gets its own row with
// the VM columns blank, and an unbound address does too.
func TestVBillingListsObjectsWithNoVM(t *testing.T) {
	inv := fixture()
	s := buildVBilling(inv, buildIndexes(inv))

	var disks, fips []string
	for _, r := range s.Rows {
		if r[0] != unassigned {
			continue
		}
		if r[1] != "" || r[2] != "" {
			t.Errorf("unassigned row carries a flavor or OS: %v", r)
		}
		if r[3] != nil {
			disks = append(disks, fmt.Sprint(r[3]))
		}
		if r[6] != "" {
			fips = append(fips, fmt.Sprint(r[6]))
		}
	}
	// 150 available, 25 stuck in error: both hold capacity.
	if fmt.Sprint(disks) != "[150 25]" {
		t.Errorf("VM-less disks = %v, want [150 25]", disks)
	}
	// fip3 is unbound; fip2 is bound to a router port, so still has no VM.
	if fmt.Sprint(fips) != "[203.0.113.11 203.0.113.12]" {
		t.Errorf("VM-less floating IPs = %v, want both", fips)
	}
	// VMs first, then the rows without one.
	for i, r := range s.Rows {
		if r[0] == unassigned && i < len(inv.Servers) {
			t.Error("unassigned rows must sort below the VMs")
		}
		if r[0] == "" {
			t.Errorf("row %d has a blank VM cell; every row must be keyed", i)
		}
	}
}
