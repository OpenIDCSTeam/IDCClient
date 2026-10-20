; ============================================================================
; OpenIDCS HostAgent (Go) Windows 安装脚本（Inno Setup 6）
;
; 本地构建:
;   1) go build -trimpath -ldflags "-s -w" -o dist\hostagent.exe .\cmd\server
;   2) 下载 NSSM 2.24，把 win64\nssm.exe 放到本文件同目录
;   3) "%ProgramFiles(x86)%\Inno Setup 6\ISCC.exe" /DAppVersion=1.0.0 hostagent.iss
;
; 产物: BuildCache\windows\OpenIDCS-HostAgent-<版本>-win-x64-setup.exe
;
; 安装时以 LocalSystem 注册为 Windows 服务 OpenIDCS-HostAgent，
; 数据目录固定为 C:\ProgramData\OpenIDCS\HostAgent\DataSaving（升级不丢失）。
; ============================================================================

#ifndef AppVersion
#define AppVersion "0.0.0"
#endif
#ifndef HttpPort
#define HttpPort "1881"
#endif

#define AppName "OpenIDCS HostAgent"
#define AppPublisher "OpenIDCSTeam"
#define AppURL "https://github.com/OpenIDCSTeam/IDCClient"
#define ServiceName "OpenIDCS-HostAgent"
#define ServiceDisplayName "OpenIDCS HostAgent"

[Setup]
AppId={{8E1F3B62-7C4A-4E21-9F6D-2B5A7C9D4E10}
AppName={#AppName}
AppVersion={#AppVersion}
AppVerName={#AppName} {#AppVersion}
AppPublisher={#AppPublisher}
AppPublisherURL={#AppURL}
AppSupportURL={#AppURL}
AppUpdatesURL={#AppURL}
DefaultDirName={autopf}\OpenIDCS\HostAgent
DisableProgramGroupPage=yes
OutputDir=..\..\BuildCache\windows
OutputBaseFilename=OpenIDCS-HostAgent-{#AppVersion}-win-x64-setup
Compression=lzma2/max
SolidCompression=yes
WizardStyle=modern
PrivilegesRequired=admin
ArchitecturesAllowed=x64compatible
ArchitecturesInstallIn64BitMode=x64compatible
UninstallDisplayName={#AppName}
; 卸载时不要自动删除用户数据目录
UninstallDisplayIcon={app}\hostagent.exe

[Languages]
Name: "chinesesimp"; MessagesFile: "compiler:Languages\ChineseSimplified.isl"
Name: "english"; MessagesFile: "compiler:Default.isl"

[Tasks]
Name: "firewall"; Description: "Open Web / VNC / SSH terminal ports in Windows Firewall"; GroupDescription: "Network:"

[Files]
Source: "..\..\dist\hostagent.exe"; DestDir: "{app}"; Flags: ignoreversion
Source: "..\..\static\*"; DestDir: "{app}\static"; Flags: ignoreversion recursesubdirs createallsubdirs
Source: "nssm.exe"; DestDir: "{app}"; Flags: ignoreversion

[Dirs]
Name: "{commonappdata}\OpenIDCS\HostAgent\DataSaving"

[Icons]
Name: "{group}\{#AppName} 管理界面"; Filename: "http://127.0.0.1:{#HttpPort}"
Name: "{group}\卸载 {#AppName}"; Filename: "{uninstallexe}"

[Code]
const
  ServiceName = '{#ServiceName}';

var
  NssmPath: string;

function DataDir: string;
begin
  Result := ExpandConstant('{commonappdata}\OpenIDCS\HostAgent\DataSaving');
end;

function RunNssm(const Params: string): Boolean;
var
  ResultCode: Integer;
begin
  Result := Exec(NssmPath, Params, '', SW_HIDE, ewWaitUntilTerminated, ResultCode);
end;

// 升级前先停掉并删除旧服务，否则 hostagent.exe 被占用且 nssm install 会失败
function PrepareToInstall(var NeedsRestart: Boolean): String;
begin
  Result := '';
  NssmPath := ExpandConstant('{app}\nssm.exe');
  if FileExists(NssmPath) then
  begin
    RunNssm('stop ' + ServiceName);
    RunNssm('remove ' + ServiceName + ' confirm');
  end;
end;

procedure CurStepChanged(CurStep: TSetupStep);
var
  ResultCode: Integer;
begin
  if CurStep = ssPostInstall then
  begin
    ForceDirectories(DataDir);
    NssmPath := ExpandConstant('{app}\nssm.exe');

    RunNssm('install ' + ServiceName + ' "' + ExpandConstant('{app}\hostagent.exe') + '"');
    RunNssm('set ' + ServiceName + ' AppDirectory "' + ExpandConstant('{app}') + '"');
    RunNssm('set ' + ServiceName + ' AppParameters --port {#HttpPort} --data-dir "' + DataDir + '"');
    RunNssm('set ' + ServiceName + ' DisplayName "' + '{#ServiceDisplayName}' + '"');
    RunNssm('set ' + ServiceName + ' Description "OpenIDCS HostAgent - virtualization management backend"');
    RunNssm('set ' + ServiceName + ' Start SERVICE_AUTO_START');
    RunNssm('set ' + ServiceName + ' AppExit Default Restart');
    RunNssm('set ' + ServiceName + ' AppRestartDelay 5000');
    RunNssm('set ' + ServiceName + ' AppStdout "' + DataDir + '\service-stdout.log"');
    RunNssm('set ' + ServiceName + ' AppStderr "' + DataDir + '\service-stderr.log"');
    RunNssm('set ' + ServiceName + ' AppRotateFiles 1');
    RunNssm('start ' + ServiceName);

    if WizardIsTaskSelected('firewall') then
      Exec('netsh', 'advfirewall firewall add rule name="OpenIDCS HostAgent" dir=in action=allow protocol=TCP localport={#HttpPort},6080,7681',
        '', SW_HIDE, ewWaitUntilTerminated, ResultCode);
  end;
end;

procedure CurUninstallStepChanged(CurUninstallStep: TUninstallStep);
var
  ResultCode: Integer;
begin
  if CurUninstallStep = usUninstall then
  begin
    NssmPath := ExpandConstant('{app}\nssm.exe');
    if FileExists(NssmPath) then
    begin
      RunNssm('stop ' + ServiceName);
      RunNssm('remove ' + ServiceName + ' confirm');
    end;
    Exec('netsh', 'advfirewall firewall delete rule name="OpenIDCS HostAgent"',
      '', SW_HIDE, ewWaitUntilTerminated, ResultCode);
  end;
end;
