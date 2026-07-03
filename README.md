# Filebrowser

A single-binary, zero-dependency web file manager. Browse, upload, download, and manage files from any browser.

## Quick Start

Download the binary for your platform from the [Releases page](https://github.com/tayyebi/filebrowser/releases), place it in the folder you want to serve, and run it:

**Linux / macOS:**
```sh
chmod +x filebrowser-linux-amd64
./filebrowser-linux-amd64
```

**Windows:**
```
filebrowser-windows-amd64.exe
```

Open **http://localhost:8080** in your browser and sign in with the default credentials (`admin` / `admin`).

> A `.env` file is created automatically on first run with the defaults. Edit it to change the address, port, or credentials, then restart.

## Features

- **Browse** folders with breadcrumb navigation and file-type icons
- **Upload** files of any size — sent in chunks with pause, resume, and cancel
- **Resume interrupted uploads** — pick up where you left off even after a browser restart
- **Download** files with streaming progress
- **Delete** files and folders with a confirmation step
- **Protected paths** — the app binary and `.env` file cannot be deleted or overwritten
- **Single binary** — no dependencies, no runtime, no database
- **Session authentication** — cookie-based with 24-hour expiry

## Configuration

All settings live in a `.env` file next to the binary:

| Variable   | Default   | Description                  |
|------------|-----------|------------------------------|
| `HOST`     | `0.0.0.0` | Network interface to bind to |
| `PORT`     | `8080`    | TCP port                     |
| `USERNAME` | `admin`   | Login username               |
| `PASSWORD` | `admin`   | Login password               |

After editing `.env`, restart the server for changes to take effect.

## Using It

### Sign in

Use the credentials from your `.env` file. Sessions last 24 hours.

### Browse and navigate

- The **breadcrumb bar** at the top shows your current path — click any segment to jump there.
- Click a **folder name** to open it. Use the **..** row to go up.
- File size and modification date columns appear on screens wider than 640 px.

### Upload

1. Pick one or more files using the upload area at the bottom.
2. Click **Upload**.
3. Track progress in the **Transfers** panel — pause, resume, or cancel any transfer.

If a large upload is interrupted, the partial file appears with a **▶ Resume** button next to it. Click it, re-select the original file, and the upload continues from where it stopped.

### Download

Click the **⬇** button on any file row. Downloads stream in the background and save automatically when complete.

### Delete

Click the **✕** button and confirm. The app will not let you delete itself or the `.env` file.

## Building From Source

Requires Go 1.22 or later.

```sh
go build -ldflags="-s -w" -o filebrowser .
```

The binary is fully self-contained — HTML templates are embedded at compile time.

## License

MIT
