{
  description = "A very basic flake";

  inputs = {
    nixpkgs.url = "github:nixos/nixpkgs/nixpkgs-unstable";
  };

  outputs = { self, nixpkgs }:
  let
    system = "x86_64-linux";
    pkgs = import nixpkgs { inherit system; };
    ciStdenv = pkgs.stdenv // {
      mkDerivation = args:
        let
          # Define our preBuild additions once
          ciPreBuild = original: ''
            ${original}
            # CI/Non-interactive environment
            export CI=true
            export DEBIAN_FRONTEND=noninteractive
            export NONINTERACTIVE=1

            # Terminal and color control
            export TERM=dumb
            export NO_COLOR=1
            export FORCE_COLOR=0
            export CLICOLOR=0
            export CLICOLOR_FORCE=0

            # Disable progress bars and spinners
            export PROGRESS_NO_TRUNC=1
          '';

          # Wrapper that adds CI environment to any attribute set
          addCIEnv = attrs: attrs // {
            preBuild = ciPreBuild (attrs.preBuild or "");
          };

          # Wrap the args appropriately
          wrappedArgs =
            if builtins.isFunction args
            then (finalAttrs: addCIEnv (args finalAttrs))
            else addCIEnv args;
        in
        pkgs.stdenv.mkDerivation wrappedArgs;
    };
  in
   {

    packages.x86_64-linux.default = pkgs.lib.callPackageWith (
      pkgs // { stdenv = ciStdenv; }) ./package.nix {};
    packages.x86_64-linux.nixpkgs-src = nixpkgs.outPath;

    # VM configuration for script-based testing (simpler, stateless)
    nixosConfigurations.vm-script = nixpkgs.lib.nixosSystem {
      inherit system;
      modules = [
        ({ config, modulesPath, ... }: {
          # Include the package being tested
          # Import packages configuration (written by Python in flake directory)
          environment.systemPackages =
            let
              pkg = self.packages.${system}.default;
              # Import the packages file with pkg and pkgs in scope
              result = builtins.scopedImport { inherit pkgs pkg; } ./packages.nix;
              # Default utilities commonly needed for testing packages
              defaultUtils = with pkgs; [
                bash
                coreutils
                # Text processing and inspection
                findutils
                gnugrep
                gnused
                gawk
                which
                file
                tree
                # Process and system inspection
                procps
                util-linux
                # Network utilities (useful even without network access)
                iproute2
                # Compression and archives
                gzip
                bzip2
                xz
                unzip
                zip
                # Version control (for packages that use it in tests)
                git
              ];
            in
              # Validate that the result is a list (safety check against sandbox escapes)
              assert builtins.isList result;
              result ++ defaultUtils;

          # Create symlink to the package in test user's home
          system.activationScripts.packageSymlink = ''
            mkdir -p /home/test
            ln -sfn ${self.packages.${system}.default} /home/test/package
            chown -h test:users /home/test/package
          '';

          # Basic VM settings
          virtualisation = {
            graphics = false;
          };

          users.users.test = {
            isNormalUser = true;
            extraGroups = [ "wheel" ];
            password = "test";
          };
          users.users.root.hashedPassword = "*"; # Disable root user
          users.mutableUsers = false;

          # CI/Non-interactive environment variables
          environment.sessionVariables = {
            CI = "true";
            DEBIAN_FRONTEND = "noninteractive";
            NONINTERACTIVE = "1";
            TERM = "dumb";
            NO_COLOR = "1";
            FORCE_COLOR = "0";
            CLICOLOR = "0";
            CLICOLOR_FORCE = "0";
            PROGRESS_NO_TRUNC = "1";
          };

          # Prevent mounting home directory for better isolation
          nixos-shell.mounts.mountHome = false;

          # Mount shared folder for script execution
          nixos-shell.mounts.extraMounts = {
            "/task" = {
              target = "{{ flake_dir }}/vm-task";
              cache = "none";  # see writes immediately
            };
          };

          # Disable network access for security
          virtualisation.qemu.networkingOptions = [];

          # Disable Nix daemon but keep basic utilities
          nix.enable = false;

          # Enable sudo without password for testing
          security.sudo.wheelNeedsPassword = false;

          # Systemd service to run the task script and shutdown
          systemd.services.run-task = {
            wantedBy = [ "multi-user.target" ];
            # Ensure the mount is available before running
            requires = [ "task.mount" ];
            after = [ "multi-user.target" "task.mount" ];
            serviceConfig = {
              Type = "oneshot";
              User = "root"; # Need root to write to /task and poweroff
            };
            # Set PATH to include all system packages (reuse systemPackages definition)
            path = config.environment.systemPackages;
            script = ''
              set -x
              # Wait for /task to be available
              for i in {1..10}; do
                if [ -d /task ]; then
                  break
                fi
                sleep 1
              done

              echo "Starting task execution" > /task/output.txt 2>&1 || {
                echo "Failed to write to /task/output.txt" >&2
                systemctl poweroff
                exit 1
              }

              if [ -f /task/run.sh ]; then
                chmod +x /task/run.sh
                /bin/sh /task/run.sh >> /task/output.txt 2>&1 || echo "Script exited with code $?" >> /task/output.txt
              else
                echo "Error: /task/run.sh not found" >> /task/output.txt
              fi
              sync  # Ensure output is written
              systemctl poweroff
            '';
          };

          # Safety timeout: force shutdown if VM hasn't completed within timeout
          systemd.timers.vm-timeout = {
            wantedBy = [ "timers.target" ];
            timerConfig = {
              OnBootSec = "{{ vm_timeout }}s";
              AccuracySec = "1s";
            };
          };

          systemd.services.vm-timeout = {
            serviceConfig = {
              Type = "oneshot";
            };
            script = ''
              echo "VM timeout reached, forcing shutdown" >> /task/output.txt 2>&1 || true
              sync
              systemctl poweroff --force
            '';
          };
        })
      ];
    };

  };
}
