# Contributing

Build and test it with Go 1.27 (`direnv allow` or `nix develop` provides it):

```sh
go test ./...
go vet ./...
gofmt -l .
go run github.com/llehouerou/oiko/cmd/oiko-build@latest -with github.com/llehouerou/oiko-ddwrt=. -o oiko
```

The tests run against a fake router in memory: no router, network or real clock. Test data
uses made-up names and addresses, never a real router's.

Everything else, from when an issue comes first to the pull request terms, is
[Oiko's CONTRIBUTING](https://github.com/llehouerou/oiko/blob/main/CONTRIBUTING.md), and everyone
taking part follows its [code of conduct](https://github.com/llehouerou/oiko/blob/main/CODE_OF_CONDUCT.md).
A contribution comes in under this repository's [license](LICENSE), Apache-2.0.

## Releasing (maintainer)

Releases are tags `vX.Y.Z` on `main`, after `go test ./...` passes, and follow
[Oiko's compatibility rules](https://github.com/llehouerou/oiko/blob/main/docs/adr/0019-release-and-compatibility-policy.md).
Each Oiko minor release ends with this repository moved to it (`go get github.com/llehouerou/oiko@vX.Y.0`)
and tagged.
