{
  lib,
  buildGoModule,
}:
let
  version = "0.0.0";

  # https://github.com/NixOS/nixpkgs/pull/359641
  enableCGO =
    if builtins.hasAttr "CGO_ENABLED" (lib.functionArgs buildGoModule) then
      { CGO_ENABLED = 0; }
    else
      { env.CGO_ENABLED = 0; };
in
buildGoModule (
  {
    pname = "prometheus-github-exporter";
    inherit version;

    src = lib.sources.sourceByRegex ./. [
      ".*\.go$"
      "^go.mod$"
      "^go.sum$"
    ];

    vendorHash = "sha256-cHEuhjvjSQBXLGQwZ/GXm9G4M976BvDPrc7fdz96cd0=";

    ldflags = [
      "-s"
      "-w"
    ];

    meta = {
      description = "Prometheus exporter for GitHub metrics";
      homepage = "https://github.com/josh/github_exporter";
      license = lib.licenses.mit;
      platforms = lib.platforms.all;
      mainProgram = "github_exporter";
    };
  }
  // enableCGO
)
