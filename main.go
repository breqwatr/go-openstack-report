package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/xuri/excelize/v2"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack"
	"github.com/gophercloud/gophercloud/v2/openstack/blockstorage/v3/volumes"
	"github.com/gophercloud/gophercloud/v2/openstack/compute/v2/flavors"
	"github.com/gophercloud/gophercloud/v2/openstack/compute/v2/servers"
	"github.com/gophercloud/gophercloud/v2/openstack/networking/v2/extensions/layer3/floatingips"
	"github.com/gophercloud/gophercloud/v2/openstack/networking/v2/ports"
)

var flavorRegex = regexp.MustCompile(`v\d+\.c(\d+)r(\d+)`)

type FixedServer struct {
	servers.Server
	Addresses map[string]interface{} `json:"addresses"`
}

type Report struct {
	VMName      string
	Flavor      string
	License     string
	VMID        string
	DiskSizes   []int
	FloatingIPs []string
}

type Summary struct {
	VMCount            int
	TotalStorage       int
	UnallocatedStorage int
	TotalFloatingIPs   int
	LicenseCounts      map[string]int
	TotalVCPUs         int
	TotalRAM           int
}

func main() {
	ctx := context.Background()

	opts, err := openstack.AuthOptionsFromEnv()
	if err != nil {
		log.Fatalf("Failed to get auth options: %v", err)
	}

	if os.Getenv("OS_PROJECT_DOMAIN_ID") != "" {
		opts.DomainID = os.Getenv("OS_PROJECT_DOMAIN_ID")
	} else {
		opts.DomainName = os.Getenv("OS_PROJECT_DOMAIN_NAME")
	}

	provider, err := openstack.AuthenticatedClient(ctx, opts)
	if err != nil {
		log.Fatalf("Failed to authenticate: %v", err)
	}

	computeClient, _ := openstack.NewComputeV2(provider, gophercloud.EndpointOpts{})
	storageClient, _ := openstack.NewBlockStorageV3(provider, gophercloud.EndpointOpts{})
	networkClient, _ := openstack.NewNetworkV2(provider, gophercloud.EndpointOpts{})

	allVMs, _ := listFixedServers(ctx, computeClient)
	allFlavors, _ := listFlavors(ctx, computeClient)
	allPorts, _ := listPorts(ctx, networkClient)
	allVolumes, _ := listVolumes(ctx, storageClient)
	allFIPs, _ := listFloatingIPs(ctx, networkClient)

	flavorMap := make(map[string]string)
	for _, flavor := range allFlavors {
		flavorMap[flavor.ID] = flavor.Name
	}

	portToFIP := make(map[string][]string)
	for _, fip := range allFIPs {
		if fip.PortID != "" {
			portToFIP[fip.PortID] = append(portToFIP[fip.PortID], fip.FloatingIP)
		}
	}

	unallocatedStorage, unallocatedDisks, unallocatedDiskSizes := calculateUnallocatedStorage(allVolumes)
	report := generateReport(allVMs, allPorts, allVolumes, portToFIP, flavorMap)

	summaryData := generateSummary(report, unallocatedStorage, unallocatedDisks, unallocatedDiskSizes)
	writeExcel(report, unallocatedStorage, unallocatedDisks, unallocatedDiskSizes, summaryData)
}

func listFixedServers(ctx context.Context, client *gophercloud.ServiceClient) ([]FixedServer, error) {
	allPages, err := servers.List(client, nil).AllPages(ctx)
	if err != nil {
		return nil, err
	}

	var allServers []FixedServer
	err = servers.ExtractServersInto(allPages, &allServers)
	return allServers, err
}

func listFlavors(ctx context.Context, client *gophercloud.ServiceClient) ([]flavors.Flavor, error) {
	allPages, err := flavors.ListDetail(client, nil).AllPages(ctx)
	if err != nil {
		return nil, err
	}
	return flavors.ExtractFlavors(allPages)
}

func listPorts(ctx context.Context, client *gophercloud.ServiceClient) ([]ports.Port, error) {
	allPages, err := ports.List(client, nil).AllPages(ctx)
	if err != nil {
		return nil, err
	}
	return ports.ExtractPorts(allPages)
}

func listVolumes(ctx context.Context, client *gophercloud.ServiceClient) ([]volumes.Volume, error) {
	allPages, err := volumes.List(client, nil).AllPages(ctx)
	if err != nil {
		return nil, err
	}
	return volumes.ExtractVolumes(allPages)
}

func listFloatingIPs(ctx context.Context, client *gophercloud.ServiceClient) ([]floatingips.FloatingIP, error) {
	allPages, err := floatingips.List(client, nil).AllPages(ctx)
	if err != nil {
		return nil, err
	}
	return floatingips.ExtractFloatingIPs(allPages)
}

func calculateUnallocatedStorage(vols []volumes.Volume) (int, []string, []int) {
	totalUnallocated := 0
	var unallocatedDisks []string
	var unallocatedDiskSizes []int

	for _, vol := range vols {
		if len(vol.Attachments) == 0 {
			totalUnallocated += vol.Size
			unallocatedDisks = append(unallocatedDisks, vol.Name)
			unallocatedDiskSizes = append(unallocatedDiskSizes, vol.Size)
		}
	}
	return totalUnallocated, unallocatedDisks, unallocatedDiskSizes
}

// generateReport correctly identifies boot disks and sorts extra volumes.
func generateReport(vms []FixedServer, ports []ports.Port, vols []volumes.Volume, portToFIP map[string][]string, flavorMap map[string]string) []Report {
	var report []Report

	for _, vm := range vms {
		licenseType := ""
		if meta, exists := vm.Metadata["license_type"]; exists {
			licenseType = fmt.Sprintf("%v", meta)
		}

		flavor := flavorMap[vm.Flavor["id"].(string)]
		var diskSizes []int
		var floatingIPs []string
		var bootDiskSize int
		var extraDiskSizes []int

		var attachedVolumes []volumes.Volume

		for _, vol := range vols {
			for _, attachment := range vol.Attachments {
				if attachment.ServerID == vm.ID {
					attachedVolumes = append(attachedVolumes, vol)
				}
			}
		}

		if len(attachedVolumes) > 0 {
			sort.Slice(attachedVolumes, func(i, j int) bool {
				return attachedVolumes[i].Name < attachedVolumes[j].Name
			})

			bootDiskSize = attachedVolumes[0].Size
			for i, vol := range attachedVolumes {
				if i == 0 {
					continue
				}
				extraDiskSizes = append(extraDiskSizes, vol.Size)
			}
		}

		if bootDiskSize > 0 {
			diskSizes = append(diskSizes, bootDiskSize)
		}

		sort.Ints(extraDiskSizes)
		diskSizes = append(diskSizes, extraDiskSizes...)

		for _, port := range ports {
			if port.DeviceID == vm.ID {
				if ips, exists := portToFIP[port.ID]; exists {
					floatingIPs = append(floatingIPs, ips...)
				}
			}
		}

		report = append(report, Report{
			VMID:        vm.ID,
			VMName:      vm.Name,
			Flavor:      flavor,
			License:     licenseType,
			DiskSizes:   diskSizes,
			FloatingIPs: floatingIPs,
		})
	}
	return report
}

func generateSummary(report []Report, unallocatedStorage int, unallocatedDisks []string, unallocatedDiskSizes []int) Summary {
	summary := Summary{
		VMCount:            len(report),
		TotalStorage:       0,
		UnallocatedStorage: unallocatedStorage,
		TotalFloatingIPs:   0,
		LicenseCounts:      make(map[string]int),
		TotalVCPUs:         0,
		TotalRAM:           0,
	}

	// Loop through each VM in the report
	for _, entry := range report {
		summary.TotalStorage += sum(entry.DiskSizes)
		summary.TotalFloatingIPs += len(entry.FloatingIPs)

		// Count licenses
		license := entry.License
		if license == "" || license == "null" {
			license = "no-OS"
		}
		summary.LicenseCounts[license]++

		matches := flavorRegex.FindStringSubmatch(entry.Flavor)
		if len(matches) == 3 {
			vCPUs, _ := strconv.Atoi(matches[1])
			RAM, _ := strconv.Atoi(matches[2])
			summary.TotalVCPUs += vCPUs
			summary.TotalRAM += RAM
		}
	}

	return summary
}

func sum(arr []int) int {
	total := 0
	for _, v := range arr {
		total += v
	}
	return total
}

func excelColumn(n int) string {
	letters := ""
	for n > 0 {
		n--
		letters = string('A'+(n%26)) + letters
		n /= 26
	}
	return letters
}

func writeExcel(report []Report, unallocatedStorage int, unallocatedDisks []string, unallocatedDiskSizes []int, summaryData Summary) {
	timestamp := time.Now().Format("2006-01-02")
	filename := fmt.Sprintf("report-%s.xlsx", timestamp)

	f := excelize.NewFile()

	// Create Summary sheet
	summarySheet := "Summary"
	summaryIndex, err := f.NewSheet(summarySheet)
	if err != nil {
		log.Fatalf("Failed to create summary sheet: %v", err)
	}

	// Generate Summary Data
	f.SetCellValue(summarySheet, "A1", "Summary Report")
	f.SetCellValue(summarySheet, "A3", "Total VM Count")
	f.SetCellValue(summarySheet, "B3", summaryData.VMCount)
	f.SetCellValue(summarySheet, "A4", "Total Storage (GB)")
	f.SetCellValue(summarySheet, "B4", summaryData.TotalStorage)
	f.SetCellValue(summarySheet, "A5", "Unallocated Storage (GB)")
	f.SetCellValue(summarySheet, "B5", unallocatedStorage)
	f.SetCellValue(summarySheet, "A6", "Total Floating IPs")
	f.SetCellValue(summarySheet, "B6", summaryData.TotalFloatingIPs)
	f.SetCellValue(summarySheet, "A7", "Total vCPUs")
	f.SetCellValue(summarySheet, "B7", summaryData.TotalVCPUs)
	f.SetCellValue(summarySheet, "A8", "Total RAM (GB)")
	f.SetCellValue(summarySheet, "B8", summaryData.TotalRAM)

	// Write License Counts
	f.SetCellValue(summarySheet, "A10", "License Type")
	f.SetCellValue(summarySheet, "B10", "Count")

	row := 11
	for license, count := range summaryData.LicenseCounts {
		f.SetCellValue(summarySheet, fmt.Sprintf("A%d", row), license)
		f.SetCellValue(summarySheet, fmt.Sprintf("B%d", row), count)
		row++
	}

	vmSheet := "VM Report"
	_, err = f.NewSheet(vmSheet)
	if err != nil {
		log.Fatalf("Failed to create VM report sheet: %v", err)
	}

	header := []string{"VM Name", "Flavor", "License", "VM ID"}
	maxDisks := 0
	for _, entry := range report {
		if len(entry.DiskSizes) > maxDisks {
			maxDisks = len(entry.DiskSizes)
		}
	}
	for i := 1; i <= maxDisks; i++ {
		header = append(header, fmt.Sprintf("Disk %d Size (GB)", i))
	}
	header = append(header, "Floating IPs")

	for col, h := range header {
		f.SetCellValue(vmSheet, excelColumn(col+1)+"1", h)
	}

	for row, entry := range report {
		rowNum := row + 2
		f.SetCellValue(vmSheet, excelColumn(1)+strconv.Itoa(rowNum), entry.VMName)
		f.SetCellValue(vmSheet, excelColumn(2)+strconv.Itoa(rowNum), entry.Flavor)
		f.SetCellValue(vmSheet, excelColumn(3)+strconv.Itoa(rowNum), entry.License)
		f.SetCellValue(vmSheet, excelColumn(4)+strconv.Itoa(rowNum), entry.VMID)

		for i, size := range entry.DiskSizes {
			f.SetCellValue(vmSheet, excelColumn(5+i)+strconv.Itoa(rowNum), size)
		}

		f.SetCellValue(vmSheet, excelColumn(5+maxDisks)+strconv.Itoa(rowNum), strings.Join(entry.FloatingIPs, ", "))
	}

	unallocatedSheet := "Unallocated Disks"
	_, err = f.NewSheet(unallocatedSheet)
	if err != nil {
		log.Fatalf("Failed to create Unallocated Disks sheet: %v", err)
	}

	f.SetCellValue(unallocatedSheet, "A1", "Unallocated Disk Name")
	f.SetCellValue(unallocatedSheet, "B1", "Size (GB)")

	for i, disk := range unallocatedDisks {
		f.SetCellValue(unallocatedSheet, fmt.Sprintf("A%d", i+2), disk)
		f.SetCellValue(unallocatedSheet, fmt.Sprintf("B%d", i+2), unallocatedDiskSizes[i])
	}

	f.SetActiveSheet(summaryIndex)

	// fuck you excel
	f.DeleteSheet("Sheet1")

	if err := f.SaveAs(filename); err != nil {
		log.Fatalf("Failed to save Excel file: %v", err)
	}
}
