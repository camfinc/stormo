using Microsoft.UI.Xaml;
using Microsoft.UI.Xaml.Controls;
using Stormo.Runtime;
using Windows.System;
using Windows.Graphics;

namespace Stormo.Desktop;

public sealed partial class MainWindow : Window
{
    private readonly LocalRuntime runtime = new(Path.Combine(AppContext.BaseDirectory, "assets"));
    private readonly DispatcherTimer timer = new() { Interval = TimeSpan.FromSeconds(5) };
    private IReadOnlyList<SetupCheck> checks = [];
    private bool busy;
    private bool ready;
    private string page = "setup";

    public MainWindow()
    {
        InitializeComponent();
        AppWindow.Resize(new SizeInt32(1120, 800));
        AppWindow.SetIcon(Path.Combine(AppContext.BaseDirectory, "assets", "stormo.ico"));
        WorkspacePath.Text = runtime.Instance;
        Navigation.SelectedItem = SetupItem;
        timer.Tick += async (_, _) => { if (!busy && ready && page == "agents") await Refresh(); };
        AppWindow.Closing += (_, args) =>
        {
            if (!busy) return;
            args.Cancel = true;
            Notice.Title = "An operation is still running";
            Notice.Message = "Wait for the current step to finish before closing Stormo.";
            Notice.Severity = InfoBarSeverity.Informational;
            Notice.IsOpen = true;
        };
        Closed += (_, _) => timer.Stop();
    }
    private async void OnLoaded(object sender, RoutedEventArgs args) { timer.Start(); await Detect(); }
    private void OnNavigationChanged(NavigationView sender, NavigationViewSelectionChangedEventArgs args)
    {
        if (args.SelectedItem is not NavigationViewItem item || SetupPage == null) return;
        page = (string)item.Tag;
        SetupPage.Visibility = page == "setup" ? Visibility.Visible : Visibility.Collapsed;
        AgentsPage.Visibility = page == "agents" ? Visibility.Visible : Visibility.Collapsed;
        CorePage.Visibility = page == "core" ? Visibility.Visible : Visibility.Collapsed;
    }
    private void UpdateControls()
    {
        CheckButton.IsEnabled = !busy;
        NextButton.IsEnabled = !busy && checks.Count > 0 && (ready || checks[^1].Key is not ("repair" or "windows"));
        StartCore.IsEnabled = StopCore.IsEnabled = RefreshAgents.IsEnabled = !busy && ready;
        StartAgent.IsEnabled = StopAgent.IsEnabled = RestartAgent.IsEnabled = NapAgent.IsEnabled = !busy && ready && AgentList.SelectedItem is Agent;
    }
    private async Task Work(string label, Func<Task> operation)
    {
        if (busy) return;
        busy = true;
        UpdateControls();
        Notice.IsOpen = false;
        Progress.Visibility = Visibility.Visible;
        Status.Text = label;
        try { await operation(); Status.Text = "Ready"; }
        catch (Exception error)
        {
            Notice.Title = "Couldn’t complete this step";
            Notice.Message = error.Message + " Check setup or retry the action.";
            Notice.Severity = InfoBarSeverity.Error;
            Notice.IsOpen = true;
            Details.Text = error.Message;
            Status.Text = "Action incomplete";
            AgentList.ItemsSource = null;
            EmptyFleet.IsOpen = false;
            FleetStatus.Text = "Couldn’t refresh your workspace. Check the core and try again.";
        }
        finally { busy = false; Progress.Visibility = Visibility.Collapsed; UpdateControls(); }
    }
    private Task Detect() => Work("Checking this PC…", DetectInner);
    private async Task DetectInner()
    {
        ready = false;
        checks = await runtime.Detect();
        Checks.ItemsSource = checks;
        ready = checks.Count > 0 && checks.All(c => c.Ready);
        var next = checks.LastOrDefault();
        NextTitle.Text = ready ? "Your workspace is ready" : next?.Title ?? "Check setup";
        Instructions.Text = next?.Detail ?? "Could not check this PC. Try again.";
        NextButton.Content = ready ? "Open workspace" : next?.Key switch { "docker" => "Get Docker Desktop", "verify" => "Test runtime", "instance" => "Create workspace", "repair" => "Repair needed", "windows" => "Unsupported PC", _ => "Check again" };
    }
    private async void OnCheck(object sender, RoutedEventArgs args) => await Detect();
    private async void OnNext(object sender, RoutedEventArgs args)
    {
        if (ready)
        {
            await Work("Opening your workspace…", async () => { await runtime.Action("core up"); await RefreshInner(); Navigation.SelectedItem = Navigation.MenuItems[1]; });
            return;
        }
        switch (checks.LastOrDefault()?.Key)
        {
            case "docker": await Work("Opening Docker’s setup guide…", async () => { if (!await Launcher.LaunchUriAsync(new Uri("https://docs.docker.com/desktop/setup/install/windows-install/"))) throw new InvalidOperationException("Could not open the Docker Desktop setup guide in your browser."); }); break;
            case "verify": await Work("Testing the runtime. The first check downloads a small image…", async () => { await runtime.VerifyContainer(); await DetectInner(); }); break;
            case "instance": await Work("Creating your workspace…", async () => { await runtime.CreateInstance(); await DetectInner(); }); break;
        }
    }
    private Task Refresh() => Work("Refreshing agents…", RefreshInner);
    private async Task RefreshInner()
    {
        var selected = (AgentList.SelectedItem as Agent)?.Id;
        var agents = await runtime.Fleet();
        AgentList.ItemsSource = agents;
        AgentList.SelectedItem = agents.FirstOrDefault(a => a.Id == selected);
        FleetStatus.Text = agents.Count == 0 ? "Your workspace is ready." : $"{agents.Count} agents in your local workspace.";
        EmptyFleet.IsOpen = agents.Count == 0;
    }
    private async void OnRefresh(object sender, RoutedEventArgs args) => await Refresh();
    private void OnAgentSelection(object sender, SelectionChangedEventArgs args) { if (StartAgent != null) UpdateControls(); }
    private async Task<bool> Confirm(string title, string message, string action)
    {
        var dialog = new ContentDialog { XamlRoot = Root.XamlRoot, Title = title, Content = message, PrimaryButtonText = action, CloseButtonText = "Cancel", DefaultButton = ContentDialogButton.Close };
        return await dialog.ShowAsync() == ContentDialogResult.Primary;
    }
    private async void OnAgentAction(object sender, RoutedEventArgs args)
    {
        if (busy || AgentList.SelectedItem is not Agent agent) return;
        var action = (string)((AppBarButton)sender).Tag;
        await Work("Updating agent…", async () =>
        {
            if ((action is "stop" or "restart") && !await Confirm($"{(action == "stop" ? "Stop" : "Restart")} {agent.Name}?", "Active work may be interrupted.", action == "stop" ? "Stop agent" : "Restart agent")) return;
            await runtime.Action(action, agent.Id);
            await RefreshInner();
        });
    }
    private async void OnStartCore(object sender, RoutedEventArgs args) => await Work("Starting core…", async () => { Details.Text = (await runtime.Action("core up")).ToString(); });
    private async void OnStopCore(object sender, RoutedEventArgs args) => await Work("Stopping core…", async () =>
    {
        if (!await Confirm("Stop the core?", "Agents stay running, but model calls and coordination pause until you start the core again.", "Stop core")) return;
        Details.Text = (await runtime.Action("core down")).ToString();
        AgentList.ItemsSource = null;
        EmptyFleet.IsOpen = false;
        FleetStatus.Text = "The core is stopped. Start it from Core, then refresh.";
    });
}
