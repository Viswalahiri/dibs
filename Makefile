GO      ?= go
PREFIX  ?= $(HOME)/.local
BIN     := $(PREFIX)/bin/dibs
CONFDIR := $(HOME)/.config/dibs
UNITDIR := $(HOME)/.config/systemd/user

define copy_once
	if [ -e $(2) ]; then echo "keeping  $(2)"; else cp $(1) $(2); echo "created  $(2)"; fi
endef

.PHONY: build test lint eval setup install service logs clean

build:
	$(GO) build -o bin/dibs ./cmd/dibs

test:
	$(GO) test ./...

lint:
	$(GO) vet ./...
	gofmt -l . | tee /dev/stderr | (! read)

# eval calls a live model against the golden fixtures, so it is never part of
# `make test`. A model version change would otherwise redden the build for a
# reason unrelated to the code.
eval:
	$(GO) test -tags eval -run TestEval ./internal/triage/...

# setup puts the config files where dibs looks for them. It never overwrites
# anything you have already edited, so it is safe to run again.
setup:
	@mkdir -p $(CONFDIR) $(HOME)/.local/share/dibs $(HOME)/.local/state/dibs
	@$(call copy_once,configs/dibs.example.yaml,$(CONFDIR)/dibs.yaml)
	@$(call copy_once,configs/repos.example.yaml,$(CONFDIR)/repos.yaml)
	@$(call copy_once,configs/env.example,$(CONFDIR)/env)
	@chmod 600 $(CONFDIR)/env
	@echo
	@echo "Next, edit these three files:"
	@echo "  $(CONFDIR)/env         paste your tokens"
	@echo "  $(CONFDIR)/dibs.yaml   set profile.github_login"
	@echo "  $(CONFDIR)/repos.yaml  list the repositories to watch"

# Written beside the target and renamed into place, because Linux refuses to
# overwrite the binary of a running process but is happy to replace its name.
install: build
	@mkdir -p $(dir $(BIN))
	cp bin/dibs $(BIN).new
	mv -f $(BIN).new $(BIN)
	-systemctl --user restart dibs

# service installs the systemd unit and enables lingering, so dibs keeps
# running after you log out.
service: install
	@mkdir -p $(UNITDIR)
	cp deploy/dibs.service $(UNITDIR)/dibs.service
	systemctl --user daemon-reload
	systemctl --user enable --now dibs
	loginctl enable-linger "$$USER"

logs:
	journalctl --user -u dibs -f

clean:
	rm -rf bin
