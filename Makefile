ifeq ($(GOPATH),)
export GOPATH=/tmp/go
endif
export PATH := $(PATH):$(GOPATH)/bin

INSTALL := /usr/bin/install
DBDIR := /var/run/redis/sonic-db/
GO ?= /usr/local/go/bin/go
TOP_DIR := $(abspath ..)
MGMT_COMMON_DIR := $(TOP_DIR)/sonic-mgmt-common
BUILD_DIR := build/bin
export CVL_SCHEMA_PATH := $(MGMT_COMMON_DIR)/build/cvl/schema
export GOBIN := $(abspath $(BUILD_DIR))

SRC_FILES=$(shell find . -name '*.go' | grep -v '_test.go' | grep -v '/tests/')
TEST_FILES=$(wildcard *_test.go)
TELEMETRY_TEST_DIR = build/tests/gnmi_server
TELEMETRY_TEST_BIN = $(TELEMETRY_TEST_DIR)/server.test
ifeq ($(TELEMETRY_WRITABLE),y)
BLD_FLAGS := -tags readwrite
endif

GO_DEPS := vendor/.done
PATCHES := $(wildcard patches/*.patch)
PATCHES += $(shell find $(MGMT_COMMON_DIR)/patches -type f)

all: sonic-telemetry $(TELEMETRY_TEST_BIN)

go.mod:
	$(GO) mod init github.com/Azure/sonic-telemetry

$(GO_DEPS): go.mod $(PATCHES)
	$(GO) mod vendor
	$(GO) mod download golang.org/x/crypto@v0.0.0-20191206172530-e9b2fee46413
	$(GO) mod download github.com/jipanyang/gnxi@v0.0.0-20181221084354-f0a90cca6fd0
	cp -r $(GOPATH)/pkg/mod/golang.org/x/crypto@v0.0.0-20191206172530-e9b2fee46413/* vendor/golang.org/x/crypto/
	cp -r $(GOPATH)/pkg/mod/github.com/jipanyang/gnxi@v0.0.0-20181221084354-f0a90cca6fd0/* vendor/github.com/jipanyang/gnxi/
	$(MGMT_COMMON_DIR)/patches/apply.sh vendor
	chmod -R u+w vendor
	patch -d vendor -p0 < patches/gnmi_cli.all.patch
	patch -d vendor -p0 < patches/gnmi_set.patch
	patch -d vendor -p0 < patches/gnmi_get.patch
	touch $@

go-deps: $(GO_DEPS)

go-deps-clean:
	$(RM) -r vendor

sonic-telemetry: $(GO_DEPS)
	$(GO) install -mod=vendor $(BLD_FLAGS) github.com/Azure/sonic-telemetry/telemetry
	$(GO) install -mod=vendor $(BLD_FLAGS) github.com/Azure/sonic-telemetry/dialout/dialout_client_cli
	$(GO) install -mod=vendor $(BLD_FLAGS) github.com/Azure/sonic-telemetry/dialout/dialout_server_cli
	$(GO) install -mod=vendor github.com/jipanyang/gnxi/gnmi_get
	$(GO) install -mod=vendor github.com/jipanyang/gnxi/gnmi_set
	$(GO) install -mod=vendor github.com/openconfig/gnmi/cmd/gnmi_cli
	$(GO) install -mod=vendor github.com/Azure/sonic-telemetry/gnoi_client

check:
	sudo mkdir -p ${DBDIR}
	sudo cp ./testdata/database_config.json ${DBDIR}
	sudo mkdir -p /usr/models/yang || true
	sudo find $(MGMT_COMMON_DIR)/models -name '*.yang' -exec cp {} /usr/models/yang/ \;
	-$(GO) test -mod=vendor $(BLD_FLAGS) -v github.com/Azure/sonic-telemetry/gnmi_server
	-$(GO) test -mod=vendor $(BLD_FLAGS) -v github.com/Azure/sonic-telemetry/dialout/dialout_client

clean:
	$(RM) -r build
	$(RM) -r vendor

$(TELEMETRY_TEST_BIN): $(TEST_FILES) $(SRC_FILES)
	mkdir -p $(@D)
	cp -r testdata $(@D)/
	$(GO) test -mod=vendor $(BLD_FLAGS) -c -cover github.com/Azure/sonic-telemetry/gnmi_server -o $@

install:
	$(INSTALL) -D $(BUILD_DIR)/telemetry $(DESTDIR)/usr/sbin/telemetry
	$(INSTALL) -D $(BUILD_DIR)/dialout_client_cli $(DESTDIR)/usr/sbin/dialout_client_cli
	$(INSTALL) -D $(BUILD_DIR)/dialout_server_cli $(DESTDIR)/usr/sbin/dialout_server_cli
	$(INSTALL) -D $(BUILD_DIR)/gnmi_get $(DESTDIR)/usr/sbin/gnmi_get
	$(INSTALL) -D $(BUILD_DIR)/gnmi_set $(DESTDIR)/usr/sbin/gnmi_set
	$(INSTALL) -D $(BUILD_DIR)/gnmi_cli $(DESTDIR)/usr/sbin/gnmi_cli
	$(INSTALL) -D $(BUILD_DIR)/gnoi_client $(DESTDIR)/usr/sbin/gnoi_client


deinstall:
	rm $(DESTDIR)/usr/sbin/telemetry
	rm $(DESTDIR)/usr/sbin/dialout_client_cli
	rm $(DESTDIR)/usr/sbin/dialout_server_cli
	rm $(DESTDIR)/usr/sbin/gnmi_get
	rm $(DESTDIR)/usr/sbin/gnmi_set
	rm $(DESTDIR)/usr/sbin/gnoi_client


