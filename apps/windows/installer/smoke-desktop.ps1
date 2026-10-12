param([string]$Executable)
$ErrorActionPreference = 'Stop'
Add-Type -AssemblyName UIAutomationClient, UIAutomationTypes, System.Drawing
Add-Type @'
using System;
using System.Runtime.InteropServices;
public static class DesktopWindow {
 [DllImport("user32.dll")] public static extern bool SetCursorPos(int x, int y);
 [DllImport("user32.dll")] public static extern bool SetForegroundWindow(IntPtr hWnd);
 [DllImport("user32.dll")] public static extern bool GetWindowRect(IntPtr hWnd, out RECT rect);
 public struct RECT { public int Left, Top, Right, Bottom; }
}
'@
$root = Split-Path $PSScriptRoot
$exe = if ($Executable) { $Executable } else { Join-Path $root 'dist/Stormo/Stormo.exe' }
$screens = Join-Path $root 'build/screens'
New-Item -ItemType Directory -Force $screens | Out-Null
$themeKey = 'HKCU:\Software\Microsoft\Windows\CurrentVersion\Themes\Personalize'
$previous = Get-ItemProperty -Path $themeKey -Name AppsUseLightTheme -ErrorAction SilentlyContinue
New-Item -Path $themeKey -Force | Out-Null
try {
 foreach ($theme in @('light', 'dark')) {
  Set-ItemProperty -Path $themeKey -Name AppsUseLightTheme -Type DWord -Value ([int]($theme -eq 'light'))
  $app = Start-Process $exe -PassThru -RedirectStandardError (Join-Path $screens "$theme-stderr.txt") -RedirectStandardOutput (Join-Path $screens "$theme-stdout.txt")
  try {
   $window = $null
   for ($i = 0; $i -lt 60; $i++) {
    Start-Sleep -Milliseconds 500
    $app.Refresh()
    if ($app.HasExited) {
     Get-Content (Join-Path $screens "$theme-stderr.txt") -ErrorAction SilentlyContinue | Write-Output
     Get-WinEvent -FilterHashtable @{LogName='Application'; StartTime=(Get-Date).AddMinutes(-2)} -ErrorAction SilentlyContinue | Where-Object { $_.ProviderName -in @('.NET Runtime', 'Application Error', 'Windows Error Reporting') } | Select-Object -First 6 -ExpandProperty Message | Write-Output
     throw "Stormo exited before showing its window: $($app.ExitCode)"
    }
    if ($app.MainWindowHandle -ne 0) {
     $window = [System.Windows.Automation.AutomationElement]::FromHandle($app.MainWindowHandle)
     $condition = [System.Windows.Automation.PropertyCondition]::new([System.Windows.Automation.AutomationElement]::AutomationIdProperty, 'SetupNav')
     if ($window.FindFirst([System.Windows.Automation.TreeScope]::Descendants, $condition)) { break }
    }
   }
   if (!$window) { throw 'Stormo did not show a native window' }
   $ready = $false
   $checkCondition = [System.Windows.Automation.PropertyCondition]::new([System.Windows.Automation.AutomationElement]::AutomationIdProperty, 'CheckAgain')
   for ($i = 0; $i -lt 90; $i++) {
    $check = $window.FindFirst([System.Windows.Automation.TreeScope]::Descendants, $checkCondition)
    if ($check -and $check.Current.IsEnabled) { $ready = $true; break }
    Start-Sleep -Milliseconds 500
   }
   if (!$ready) { throw 'Initial setup detection did not finish' }
   [DesktopWindow]::SetForegroundWindow($app.MainWindowHandle) | Out-Null
   $rect = New-Object DesktopWindow+RECT
   [DesktopWindow]::GetWindowRect($app.MainWindowHandle, [ref]$rect) | Out-Null
   [DesktopWindow]::SetCursorPos(($rect.Right - 40), ($rect.Bottom - 40)) | Out-Null
   foreach ($name in @('coreclr.dll', 'Microsoft.UI.Xaml.dll')) {
    $module = $app.Modules | Where-Object ModuleName -EQ $name | Select-Object -First 1
    if (!$module -or !$module.FileName.StartsWith((Split-Path $exe), [StringComparison]::OrdinalIgnoreCase)) { throw "$name did not load from the self-contained installation" }
    Write-Output "PASS private runtime: $name"
   }
   foreach ($page in @('Setup', 'Agents', 'Core')) {
    $condition = [System.Windows.Automation.PropertyCondition]::new([System.Windows.Automation.AutomationElement]::AutomationIdProperty, ($page + 'Nav'))
    $item = $window.FindFirst([System.Windows.Automation.TreeScope]::Descendants, $condition)
    if (!$item) { throw "$page navigation item is not accessible" }
    $pattern = $item.GetCurrentPattern([System.Windows.Automation.SelectionItemPattern]::Pattern)
    $pattern.Select()
    Start-Sleep -Seconds 2
    $rect = New-Object DesktopWindow+RECT
    [DesktopWindow]::GetWindowRect($app.MainWindowHandle, [ref]$rect) | Out-Null
    $bitmap = [System.Drawing.Bitmap]::new(($rect.Right - $rect.Left), ($rect.Bottom - $rect.Top))
    $graphics = [System.Drawing.Graphics]::FromImage($bitmap)
    try {
     $graphics.CopyFromScreen($rect.Left, $rect.Top, 0, 0, $bitmap.Size)
     $bitmap.Save((Join-Path $screens "$theme-$page.png"))
    } finally { $graphics.Dispose(); $bitmap.Dispose() }
    Write-Output "PASS native $theme $page navigation and capture"
   }
  } finally {
   if (!$app.HasExited) { $app.CloseMainWindow() | Out-Null; if (!$app.WaitForExit(10000)) { $app.Kill(); throw 'Desktop refused to close after setup checks' } }
   $app.Dispose()
  }
 }
} finally {
 if ($null -ne $previous) { Set-ItemProperty -Path $themeKey -Name AppsUseLightTheme -Value $previous.AppsUseLightTheme }
 else { Remove-ItemProperty -Path $themeKey -Name AppsUseLightTheme -ErrorAction SilentlyContinue }
}
