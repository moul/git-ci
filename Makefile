GOPKG ?=	moul.io/git-ci
DOCKER_IMAGE ?=	moul/git-ci
GOBINS ?=	.
NPM_PACKAGES ?=	.

include rules.mk

generate: install
	GO111MODULE=off go get github.com/campoy/embedmd
	mkdir -p .tmp
	echo 'foo@bar:~$$ git-ci -h' > .tmp/usage.txt
	(git-ci -h 2>&1 || true) >> .tmp/usage.txt
	embedmd -w README.md
	rm -rf .tmp
.PHONY: generate

lint:
	cd tool/lint; make
.PHONY: lint
