{
  lib,
  buildGoModule,
}:

buildGoModule {
  pname = "atlassian";
  version = "0.1.0";

  src = ../modules/programs/atlassian/atlassian;

  doCheck = true;

  vendorHash = "sha256-O4WcSqMb26E7KFSMsVykF+M4XKB3EZ2Lwhf6QXyMrNk=";

  meta = {
    description = "atlassian — read-only CLI for Jira Cloud and Confluence Cloud";
    mainProgram = "atlassian";
    license = lib.licenses.mit;
  };
}
