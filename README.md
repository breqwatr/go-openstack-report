# Go OpenStack Report Generator

This project is a Go application that generates reports for OpenStack resources. It collects information about virtual machines (VMs), flavors, ports, volumes, and floating IPs, and generates a summary report.

## Prerequisites

- Go 1.16 or later
- OpenStack credentials set in environment variables

## Installation

1. Clone the repository:

    ```sh
    git clone https://github.com/yourusername/go-openstack-report.git
    cd go-openstack-report
    ```

2. Install dependencies:

    ```sh
    make deps
    ```

## Usage

1. Build the application:

    ```sh
    make build
    ```

2. Run the application:

    ```sh
    make run
    ```

## Makefile Targets

- `make all`: Runs tests and builds the project.
- `make build`: Builds the Go project.
- `make clean`: Cleans up the build files and removes the binary.
- `make deps`: Installs the project dependencies.
- `make run`: Builds and runs the application.

## Environment Variables

Ensure the following OpenStack environment variables are set:

- `OS_AUTH_URL`
- `OS_USERNAME`
- `OS_PASSWORD`
- `OS_PROJECT_NAME`
- `OS_PROJECT_DOMAIN_ID` or `OS_PROJECT_DOMAIN_NAME`
- `OS_USER_DOMAIN_NAME`

## Workbook Layout

The tool writes `report-YYYY-MM-DD.xlsx`, scoped to the single project the token
authenticates against.

- `vBilling`: one row per VM, in the RVTools wide idiom. `VM Name`, `Flavor`,
  `OS`, then a `Disk N` column per disk in GB, then `Floating IP`. A volume with
  no VM attached gets its own row keyed `unassigned`, with just its size in
  `Disk 1`; a floating IP bound to no VM does the same. Those are billed whether
  or not anything is using them, so they are named rather than left blank: an
  empty VM cell reads as missing data, `unassigned` reads as a charge.
- `vInfo`: one row per instance, with the full detail.
- `vFlavor`: flavors and how many instances use each.
- `vDisk`: one row per volume attachment, plus unattached volumes.
- `vNetwork`: ports, fixed IPs and floating IPs.
- `vSnapshot`: volume snapshots.
- `vHealth`: derived problems worth acting on.
- `vMetaData`: provenance and per-service collection results.

### Reading vBilling

- **Disk 1 is local storage on an image-booted VM**, being the flavor root plus
  ephemeral. A volume-booted VM has no such row entry: its root is a Cinder
  volume, listed among the disks in its own right, and counting the flavor disk
  as well would bill it twice.
- **A multiattach volume appears under every VM it is attached to.** That is
  what keying on the VM means, but it does make the disk columns double count
  across rows. `vDisk` lists each volume once if you need the true total.
- **`OS` is the Glance image the VM booted**, not a guest OS. Nova does not
  report the latter. A volume-booted VM falls back to the image metadata Cinder
  copies onto the volume, and is blank if that is not set.
- **Snapshots are not on this sheet.** They belong to a volume, not a VM, and
  have no column in this shape. `vSnapshot` lists them.
- **A failed collection understates the sheet.** `vHealth` and `vMetaData` say
  which service failed, and the tool exits non-zero.

## License

This project is licensed under the Apache 2.0 License. See the LICENSE file for details.