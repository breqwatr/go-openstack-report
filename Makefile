# Go parameters
GOCMD=go
GOBUILD=$(GOCMD) build
GOCLEAN=$(GOCMD) clean
GOGET=$(GOCMD) get
BINARY_NAME=reportctl

# All target
all: build

# Build the project
build:
	$(GOBUILD) -o $(BINARY_NAME) -v

# Clean build files
clean:
	$(GOCLEAN)
	rm -f $(BINARY_NAME)

# Install dependencies
deps:
	$(GOGET) -v ./...

# Run the application
run:
	$(GOBUILD) -o $(BINARY_NAME) -v
	./$(BINARY_NAME)

.PHONY: all build clean deps run