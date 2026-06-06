{
  lib,
  buildGoModule,
}:

# The sidebar-spike — see issue #2148 and the umbrella tracking issue
# #2147 plus modules/programs/prism/prism/docs/multiplexer-proposal.md.
#
# Non-functional UI mockup of the herdr-shape sidebar planned for the
# prism-native multiplexer programme. Not production code. It is a
# sibling Go module to `prism` precisely so the spike has zero impact
# on `pkgs/prism.nix`'s `vendorHash` — adding or removing the spike
# never forces a prism rebuild.
#
# `doCheck` is hard-wired to false: the spike has no Go test suite (it
# is an interactive visual mockup whose surviving artefact is the
# design-doc addendum to `multiplexer-proposal.md`, not unit tests).
# No `runChecks` knob is exposed because there is nothing to gate on —
# symmetric with the AC's "no new CI lane" requirement.
buildGoModule {
  pname = "sidebar-spike";
  version = "0.1.0";

  src = ../modules/programs/prism/sidebar-spike;

  # Single binary at the module root.
  subPackages = [ "." ];

  doCheck = false;

  vendorHash = "sha256-uwBJAqN4sIepiiJf9lCDumLqfKJEowQO2tOiSWD3Fig=";

  ldflags = [
    "-s"
    "-w"
  ];

  meta = {
    description = "Non-functional UI mockup of the herdr-shape sidebar for the prism-native multiplexer programme (issue #2148)";
    mainProgram = "sidebar-spike";
    license = lib.licenses.mit;
  };
}
