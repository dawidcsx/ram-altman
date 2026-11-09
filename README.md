# RAM Altman — public client source (Windows & Linux)

MIT-licensed Windows and Linux telemetry agents from Mochi Software.
The global aggregation backend is proprietary and **not** included in this archive.

- `agent/` — Go Windows tray client and settings UI.
- `linux-agent/` — Go Linux client, browser settings, opt-in and installer.
- `LICENSE` — MIT licence for the **client code** in this package.

Both clients send a random device ID, total RAM and available RAM counts to `https://mochisoftware.com/api/ram/report` after opt-in.
No file contents or process lists are reported. The server and network intermediaries can see request IP addresses.

Build using Go 1.23+ as described in the source directories.

This is a parody project, unaffiliated with Sam Altman or OpenAI. Available RAM is not necessarily wasted RAM.
