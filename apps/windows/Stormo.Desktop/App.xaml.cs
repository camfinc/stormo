using Microsoft.UI.Xaml;
using System.Runtime.InteropServices;
namespace Stormo.Desktop;
public partial class App : Application
{
    private Window? window;
    [DllImport("shell32.dll", CharSet = CharSet.Unicode)]
    private static extern int SetCurrentProcessExplicitAppUserModelID(string appId);
    public App()
    {
        UnhandledException += (_, args) => Console.Error.WriteLine(args.Exception);
        DebugSettings.IsXamlResourceReferenceTracingEnabled = true;
        DebugSettings.XamlResourceReferenceFailed += (_, args) => Console.Error.WriteLine(args.Message);
        InitializeComponent();
    }
    protected override void OnLaunched(LaunchActivatedEventArgs args)
    {
        Marshal.ThrowExceptionForHR(SetCurrentProcessExplicitAppUserModelID("CAMF.Stormo.Desktop"));
        try
        {
            window = new MainWindow();
            window.Activate();
        }
        catch (Exception error) { Console.Error.WriteLine(error); throw; }
    }
}
