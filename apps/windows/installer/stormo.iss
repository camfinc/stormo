#define AppVersion "0.1.0"
[Setup]
AppId={{8A3B478E-7A5D-4AB6-A35C-07671EBF1B56}
AppName=Stormo Preview
AppVersion={#AppVersion}
AppPublisher=CAMF
DefaultDirName={localappdata}\Programs\Stormo
DefaultGroupName=Stormo
PrivilegesRequired=lowest
ArchitecturesAllowed=x64compatible
ArchitecturesInstallIn64BitMode=x64compatible
MinVersion=10.0.22000
DisableProgramGroupPage=yes
OutputDir=..\build\installer
OutputBaseFilename=Stormo-Setup-x64-preview
Compression=lzma2
SolidCompression=yes
WizardStyle=modern
UninstallDisplayIcon={app}\Stormo.exe
LicenseFile=..\..\..\LICENSE

[Files]
Source: "..\dist\Stormo\*"; DestDir: "{app}"; Flags: ignoreversion recursesubdirs createallsubdirs

[Icons]
Name: "{group}\Stormo"; Filename: "{app}\Stormo.exe"

[Run]
Filename: "{app}\Stormo.exe"; Description: "Open Stormo and set up your workspace"; Flags: nowait postinstall skipifsilent

; No uninstall action touches WSL, Docker Desktop, or %LOCALAPPDATA%\Stormo\Runtime.
; The latter contains user work and must survive an app uninstall or upgrade.
