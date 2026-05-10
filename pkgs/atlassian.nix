{
  lib,
  buildGoModule,
}:

buildGoModule {
  pname = "atlassian";
  version = "0.1.0";

  src = ../modules/programs/atlassian/atlassian;

  doCheck = true;

  vendorHash = "sha256-hocnLCzWN8srQcO3BMNkd2lt0m54Qe7sqAhUxVZlz1k=";

  meta = {
    description = "atlassian — read-only CLI for Jira Cloud and Confluence Cloud";
    mainProgram = "atlassian";
    license = lib.licenses.mit;
  };
}
