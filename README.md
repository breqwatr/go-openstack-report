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

## Summary Report

The application generates a summary report that includes:

- Total number of VMs
- Total storage used
- Unallocated storage
- Total number of floating IPs
- License counts
- Total vCPUs
- Total RAM

## License

This project is licensed under the Apache 2.0 License. See the LICENSE file for details.