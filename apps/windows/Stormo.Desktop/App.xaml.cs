using Microsoft.UI.Xaml;
using System.Runtime.InteropServices;
namespace Stormo.Desktop;
public partial class App : Application
{
    private Window? window;
    [DllImport("shell32.dll", CharSet = CharSet.Unicode)]
    private static extern int SetCurrentProcessExplicitAppUserModelID(string appId);
    public App() { InitializeComponent(); }
    protected override void OnLaunched(LaunchActivatedEventArgs args)
    {
        Marshal.ThrowExceptionForHR(SetCurrentProcessExplicitAppUserModelID("CAMF.Stormo.Desktop"));
        window = new MainWindow();
        window.Activate();
    }
}
