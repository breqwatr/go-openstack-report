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
	$(GOBUILD) -o $(BINARY_NAME) -v  # <-- ✅ TAB here

# Clean build files
clean:
	$(GOCLEAN)                       # <-- ✅ TAB here
	rm -f $(BINARY_NAME)              # <-- ✅ TAB here

# Install dependencies
deps:
	$(GOGET) -v ./...                 # <-- ✅ TAB here

# Run the application
run:
	$(GOBUILD) -o $(BINARY_NAME) -v  # <-- ✅ TAB here
	./$(BINARY_NAME)                  # <-- ✅ TAB here

.PHONY: all build clean deps run