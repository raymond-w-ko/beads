{
  # Dolt 2.6.2 from the upstream release binary, for test harnesses on
  # x86_64-linux hosts such as nixvac. nixpkgs still ships Dolt 2.5.0, below
  # upstream beads' 2.6.0 floor. Retire this flake once nixpkgs has 2.6.x.
  #
  #   nix shell path:./nix/dolt#dolt -c dolt version   # dolt version 2.6.2
  description = "Dolt 2.6.2 release binary (stopgap until nixpkgs ships 2.6.x)";

  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";

  outputs = { self, nixpkgs }:
    let
      system = "x86_64-linux";
      pkgs = nixpkgs.legacyPackages.${system};
      version = "2.6.2";

      # The release binary is statically linked, so a plain copy runs on NixOS
      # without autoPatchelf.
      dolt = pkgs.stdenvNoCC.mkDerivation {
        pname = "dolt";
        inherit version;

        src = pkgs.fetchurl {
          url = "https://github.com/dolthub/dolt/releases/download/v${version}/dolt-linux-amd64.tar.gz";
          # Release asset digest (gh release view v2.6.2 -R dolthub/dolt --json assets):
          # sha256:e27a7957b8626946a5022fe0ad6ce4bfb3c8f51544c7f1372b0e6db14c1549cf
          hash = "sha256-4np5V7hiaUalAi/grWzkv7PI9RVEx/E3Kw5tsUwVSc8=";
        };

        dontConfigure = true;
        dontBuild = true;
        dontStrip = true;
        dontPatchELF = true;

        installPhase = ''
          runHook preInstall
          install -Dm755 bin/dolt $out/bin/dolt
          install -Dm644 LICENSES $out/share/doc/dolt/LICENSES
          runHook postInstall
        '';

        doInstallCheck = true;
        installCheckPhase = ''
          HOME=$TMPDIR $out/bin/dolt version | grep -F "dolt version ${version}"
        '';

        meta = {
          description = "SQL database with Git-style versioning (upstream release binary)";
          homepage = "https://github.com/dolthub/dolt";
          license = pkgs.lib.licenses.asl20;
          mainProgram = "dolt";
          platforms = [ system ];
          sourceProvenance = [ pkgs.lib.sourceTypes.binaryNativeCode ];
        };
      };
    in
    {
      packages.${system} = {
        inherit dolt;
        default = dolt;
      };
    };
}
