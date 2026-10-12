#define AppVersion "0.2.0"
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
OutputBaseFilename=Stormo-Setup-native-x64-preview
Compression=lzma2
SolidCompression=yes
WizardStyle=modern
SetupIconFile=..\assets\stormo.ico
UninstallDisplayIcon={app}\Stormo.exe
LicenseFile=..\..\..\LICENSE

[Files]
Source: "..\dist\Stormo\*"; DestDir: "{app}"; Flags: ignoreversion recursesubdirs createallsubdirs

[InstallDelete]
; Remove the superseded Qt payload only, never user workspaces.
Type: filesandordirs; Name: "{app}\_internal"
Type: files; Name: "{app}\_internal\assets\runtime.tar.gz"
Type: files; Name: "{app}\_internal\assets\runtime.sha256"

[Icons]
Name: "{group}\Stormo"; Filename: "{app}\Stormo.exe"; IconFilename: "{app}\Stormo.exe"; AppUserModelID: "CAMF.Stormo.Desktop"

[Run]
Filename: "{app}\Stormo.exe"; Description: "Open Stormo and set up your workspace"; Flags: nowait postinstall skipifsilent

; No uninstall action touches Docker Desktop or %LOCALAPPDATA%\Stormo\Instances.
; User work survives app uninstall and upgrade. No WSL distribution is created or managed.
