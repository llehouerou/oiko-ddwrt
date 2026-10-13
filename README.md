# oiko-ddwrt

Phones on a DD-WRT router for [Oiko](https://github.com/llehouerou/oiko): the `ddwrt` type of
Bridge, added to an Oiko build like any other. It tells whether each phone you list is
connected to a DD-WRT access point, so that Oiko can bind it as a Presence source
([ADR 0050](https://github.com/llehouerou/oiko/blob/main/docs/adr/0050-a-phone-on-a-dd-wrt-router-is-a-configured-device-its-connection-a-value.md)).

It reads the router's status page, `Status_Wireless.live.asp`, every 30 seconds with the
web interface's user and password, as Home Assistant's `ddwrt` tracker does. The page lists the
clients associated with the router's radios, whatever its chipset, so a phone counts as
connected while it is associated with the Wi-Fi; the ARP table and the DHCP leases are not read.

What it follows, for each phone you list:

- whether it is connected: a `network` Function whose `connected` Capability is true while the
  phone is associated with the access point;
- whether the router answers (the Bridge online, and each phone's Availability with it). While the
  router is down or refuses the password, the phones' Availability is unknown, so their Presence
  sources are ignored rather than read as away.

Only the phones you list become Devices: a guest's phone never enters Oiko. Nothing can be
commanded, and the Bridge keeps nothing in its data directory.

## Set up the router

The Bridge signs in as the router's web interface does, with HTTP Basic Auth: use its user
(`root` by default) and password, and write the password to a file only Oiko can read. Reach the
router over `http://`, or `https://` when its certificate verifies (DD-WRT's own self-signed one
does not).

Then find each phone's MAC on this network with the `clients` command, the phone connected to the
Wi-Fi:

```sh
oiko ddwrt clients
```

```
MAC                HOST NAME
02:00:00:00:00:01  alices-phone
02:00:00:00:00:05
```

It prints the clients associated with the access point now, each with the host name of its DHCP
lease (none when the access point is no DHCP server). It only reads the router, so it runs while
Oiko serves, and needs no `phones` yet.

A phone shows a private address on each Wi-Fi network, stable on yours unless it rotates it
(iOS's *Rotate Wi-Fi Address*, or an Android phone's non-persistent randomization): turn that off
for your home network, or update its `mac` when it changes.

## Configure

The Bridge's section of Oiko's
[configuration](https://github.com/llehouerou/oiko/blob/main/docs/configure.md#bridges),
under `bridges`:

```json
{"bridges": {"ddwrt": {
  "url": "http://192.168.1.1",
  "username": "admin",
  "passwordFile": "/path/to/ddwrt-password",
  "phones": {
    "alice": {"name": "Alice's phone", "mac": "02:00:00:00:00:01"},
    "bob": {"name": "Bob's phone", "mac": "02:00:00:00:00:02"}
  }
}}}
```

- `url`: the router's web interface, `http://` or `https://`, required.
- `username`: its user, required.
- `passwordFile`: a file holding its password, required, so that the password stays out of the
  configuration.
- `phones`: each phone to follow, at least one, by a key of your choosing:
  - `name`: the phone's Name in Oiko, required;
  - `mac`: its MAC on this network, as `clients` prints it, required.

The key is the Device's address in Oiko. A phone whose MAC changed keeps its Device, its History
and its Presence source bindings when you edit its `mac` and restart Oiko; renaming its key makes
it a new Device, which a Replace gives the old one's identity.

Any other key stops Oiko from starting.

### Bind it as a Presence source

Bind each phone's `network` Function to its Person as a Presence source, with a **departure delay
of about 15 minutes**. `connected` means associated now: an idle phone drops off the Wi-Fi for a
few minutes now and then, and the delay keeps its Person home through it
([ADR 0049](https://github.com/llehouerou/oiko/blob/main/docs/adr/0049-a-presence-is-home-while-any-presence-source-says-so.md)).
An arrival always counts at once.

### Several access points

Configure one Bridge per access point, each with its own section and every phone in it:

```json
{"bridges": {
  "ap-ground-floor": {"type": "ddwrt", "url": "http://192.168.1.1", "username": "admin",
    "passwordFile": "/path/to/ddwrt-password",
    "phones": {"alice": {"name": "Alice's phone", "mac": "02:00:00:00:00:01"}}},
  "ap-upstairs": {"type": "ddwrt", "url": "http://192.168.1.2", "username": "admin",
    "passwordFile": "/path/to/ddwrt-password",
    "phones": {"alice": {"name": "Alice's phone", "mac": "02:00:00:00:00:01"}}}
}}
```

A phone is then a Device on each, and you bind all of them as its Person's Presence sources: the
Person stays home while the phone is connected to any of them, so roaming from one to the other
changes nothing.

## Add it to Oiko

Build an Oiko with this type (Go 1.27 needed):

```sh
go run github.com/llehouerou/oiko/cmd/oiko-build@latest -with github.com/llehouerou/oiko-ddwrt@latest -o oiko
./oiko -version   # lists the ddwrt type, with its module and version
```

On NixOS, override Oiko's package with this module's version, and pass the password through
`services.oiko.credentials`, which the service reads as `/run/credentials/oiko.service/<name>`:

```nix
services.oiko.package = oiko.packages.${system}.default.override {
  bridges."github.com/llehouerou/oiko-ddwrt" = "<version>"; # its latest tag
  vendorHash = "sha256-…"; # the first nix build prints it
};
services.oiko.credentials = {
  ddwrt_password = "/run/secrets/ddwrt-password";
};
services.oiko.settings.bridges.ddwrt = {
  url = "http://192.168.1.1";
  username = "admin";
  passwordFile = "/run/credentials/oiko.service/ddwrt_password";
  phones.alice = { name = "Alice's phone"; mac = "02:00:00:00:00:01"; };
};
```

Its entry in the [catalogue](https://llehouerou.github.io/oiko-catalogue/#ddwrt) gives both,
with the latest versions. Oiko's [Install](https://github.com/llehouerou/oiko/blob/main/docs/install.md)
and [Configure](https://github.com/llehouerou/oiko/blob/main/docs/configure.md#add-a-type-of-bridge)
pages say more of building and deploying it.

## Versions

Versions follow Oiko's
([ADR 0019](https://github.com/llehouerou/oiko/blob/main/docs/adr/0019-release-and-compatibility-policy.md)):
during v0, a patch release neither breaks nor adds anything, and a minor release may break
the configuration, its notes listing what changed. Each version's `go.mod` names the
minimum Oiko it needs.

## Develop

```sh
direnv allow   # or `nix develop`: Go 1.27
go test ./...
go run github.com/llehouerou/oiko/cmd/oiko-build@latest -with github.com/llehouerou/oiko-ddwrt=. -o oiko
```

The tests run against a fake router in memory, with no network and no real clock.
[CONTRIBUTING.md](CONTRIBUTING.md) says how to contribute.

## License

[Apache-2.0](LICENSE).
