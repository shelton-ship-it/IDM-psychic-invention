.PHONY: test vet fmt build-windows package
test:
	cd core && go test -race -count=1 ./...
vet:
	cd core && go vet ./...
fmt:
	cd core && gofmt -l .
build-windows:
	scripts/build-windows.sh $(VERSION)
package:
	bash scripts/package.sh $(VERSION)
