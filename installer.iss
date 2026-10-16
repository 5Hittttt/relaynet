; RelayNet Installer Script for Inno Setup
; Generates a single .exe installer with shortcuts and uninstaller.

#define MyAppName "RelayNet"
#define MyAppVersion "1.0.0"
#define MyAppPublisher "RelayNet"
#define MyAppExeName "relayclient.exe"
#define MyServerExeName "relayserver.exe"

[Setup]
AppId={{8F4E2A1B-3C5D-4E6F-9A2B-1D3E5F7A9B2C}
AppName={#MyAppName}
AppVersion={#MyAppVersion}
AppPublisher={#MyAppPublisher}
DefaultDirName={autopf}\{#MyAppName}
DefaultGroupName={#MyAppName}
DisableProgramGroupPage=yes
OutputDir=dist
OutputBaseFilename=RelayNet-Setup-{#MyAppVersion}
Compression=lzma2/ultra64
SolidCompression=yes
WizardStyle=modern
ArchitecturesAllowed=x64compatible
ArchitecturesInstallIn64BitMode=x64
UninstallDisplayIcon={app}\{#MyAppExeName}
UninstallDisplayName={#MyAppName}
PrivilegesRequired=admin

[Languages]
Name: "english"; MessagesFile: "compiler:Default.isl"

[Tasks]
Name: "desktopicon"; Description: "{cm:CreateDesktopIcon}"; GroupDescription: "{cm:AdditionalIcons}"; Flags: unchecked

[Files]
Source: "bin\relayclient.exe"; DestDir: "{app}"; Flags: ignoreversion
Source: "bin\relayserver.exe"; DestDir: "{app}"; Flags: ignoreversion

[Icons]
Name: "{group}\RelayNet Client"; Filename: "{app}\{#MyAppExeName}"
Name: "{group}\RelayNet Server"; Filename: "{app}\{#MyServerExeName}"
Name: "{group}\Uninstall RelayNet"; Filename: "{uninstallexe}"
Name: "{autodesktop}\RelayNet Client"; Filename: "{app}\{#MyAppExeName}"; Tasks: desktopicon

[Run]
Filename: "{app}\{#MyAppExeName}"; Description: "{cm:LaunchProgram,RelayNet Client}"; Flags: nowait postinstall skipifsilent

