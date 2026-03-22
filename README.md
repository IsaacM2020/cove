# 🌊 Cove

**Your personal cloud, running on your Raspberry Pi.**

Cove is a self-hosted file storage server — like Google Drive, but on hardware you own, on your home network, with no monthly fees and no third party touching your files. Upload, browse, preview, and stream your photos and videos from anywhere via [Tailscale](https://tailscale.com).

---

## What you need

- Raspberry Pi 4 or 5 (8GB recommended, 4GB works fine)
- An external hard drive or SSD for storage
- A Tailscale account (free) — for secure remote access
- About 20 minutes

---

## Step 1 — Set up your Raspberry Pi

If you already have a Pi running with an external drive mounted, skip to Step 3.

**Flash Raspberry Pi OS:**
1. Download [Raspberry Pi Imager](https://www.raspberrypi.com/software/) on your Mac/PC
2. Choose **Raspberry Pi OS Lite (64-bit)** — no desktop needed
3. Click the gear icon ⚙️ before flashing and:
   - Set a hostname (e.g. `cove-pi`)
   - Enable SSH
   - Set your username and password
   - Configure your WiFi
4. Flash to your SD card and boot the Pi

**SSH into your Pi:**
```bash
ssh pi@cove-pi.local
```

---

## Step 2 — Mount your external drive

Plug in your external drive. Find it:
```bash
lsblk
```

You'll see something like `/dev/sda1`. Get its UUID:
```bash
sudo blkid /dev/sda1
```

Create a mount point and mount it:
```bash
sudo mkdir -p /mnt/nas
sudo mount /dev/sda1 /mnt/nas
```

**Make it mount automatically on boot.** Open fstab:
```bash
sudo nano /etc/fstab
```

Add this line at the bottom (replace `YOUR-UUID` with the UUID from blkid, and replace `exfat` with `ext4` or `ntfs` if your drive is formatted differently):
```
UUID=YOUR-UUID  /mnt/nas  exfat  defaults,nofail  0  0
```

Save with `Ctrl+X → Y → Enter`. Test it:
```bash
sudo mount -a
df -h /mnt/nas
```

You should see your drive's capacity listed.

---

## Step 3 — Install Tailscale

Tailscale gives you a secure private network so you can access Cove from anywhere — your phone, laptop, anywhere — without opening ports on your router.

```bash
curl -fsSL https://tailscale.com/install.sh | sh
sudo tailscale up
```

Follow the link it prints to authenticate. Once connected, get your Pi's Tailscale hostname:
```bash
tailscale status
```

You'll see something like `cove-pi.tail2c26ee.ts.net`. Write that down — you'll use it to access Cove.

---

## Step 4 — Install Cove

```bash
curl -fsSL https://raw.githubusercontent.com/IsaacM2020/cove/main/install.sh | sudo bash
```

The installer will ask you:
- **Password** — what you'll use to log into Cove (min 8 characters)
- **Storage directory** — where your drive is mounted (default: `/mnt/nas`)
- **Port** — which port to run on (default: `8080`)
- **Install ffmpeg?** — recommended, fixes video buffering (type Y)

That's it. Cove installs, sets itself up as a service that starts on boot, and starts running.

---

## Step 5 — Open Cove

On any device connected to your Tailscale network, open a browser and go to:

```
http://YOUR-PI-TAILSCALE-NAME:8080
```

For example: `http://cove-pi.tail2c26ee.ts.net:8080`

Log in with the password you set during install. You're in.

---

## Using Cove

**Uploading files:**
- Click **↑ Upload** → Files or Folder
- Drag and drop files directly onto the page
- Large files (tens of GB) upload reliably — uploads are sequential so each file gets full bandwidth
- Use ⏸ to pause an upload and ▶ to resume it later
- You can close the tab and come back — paused uploads resume from where they left off

**Browsing:**
- Switch between list and grid view with the buttons in the top right
- In grid view, video thumbnails load lazily as you scroll — fast even in folders with hundreds of videos
- Sort by name, size, or date
- Right-click any file or folder for options: open, download, rename, info, move to bin

**Previewing:**
- Click any photo, video, PDF, or audio file to preview it in-place
- Arrow keys navigate between files in the preview
- ESC closes the preview
- Videos start playing immediately with no buffering (thanks to automatic ffmpeg optimization on upload)

**Folders:**
- Click **+ New folder** to create one
- Drag files onto folders to move them
- Drag files onto breadcrumbs to move them up the folder tree

**Trash:**
- Deleting moves files to the bin, not permanently
- Click 🗑 **Bin** in the header to restore or permanently delete

---

## Managing Cove on your Pi

```bash
# Check if Cove is running
sudo systemctl status cove

# View live logs
sudo journalctl -u cove -f

# Restart Cove
sudo systemctl restart cove

# Stop Cove
sudo systemctl stop cove
```

**Change your password or settings:**
```bash
sudo nano /opt/cove/cove.env
sudo systemctl restart cove
```

**Check disk space:**
```bash
df -h /mnt/nas
```

---

## Updating Cove

Re-run the install script — it detects your existing config and upgrades in-place:

```bash
curl -fsSL https://raw.githubusercontent.com/IsaacM2020/cove/main/install.sh | sudo bash
```

---

## Fix buffering on existing videos

If you had videos on your drive before installing Cove, run this once to optimize them for instant playback. New uploads are handled automatically:

```bash
# Run in a tmux session so it survives disconnection
tmux new -s faststart

find /mnt/nas -name "*.mov" -o -name "*.mp4" | while read f; do
  ffmpeg -i "$f" -c copy -movflags +faststart "${f}.tmp" && mv "${f}.tmp" "$f"
done
```

Detach from tmux with `Ctrl+B, D` and let it run overnight. It's safe to interrupt — any file it hasn't touched yet is unchanged.

---

## Troubleshooting

**Can't connect to Cove:**
- Make sure both your device and the Pi are on the same Tailscale network: `tailscale status` on the Pi
- Check Cove is running: `sudo systemctl status cove`
- Check the port isn't blocked: `sudo journalctl -u cove -n 20`

**Drive not mounting after reboot:**
- Check fstab entry: `sudo cat /etc/fstab`
- Try mounting manually: `sudo mount -a` and look for errors
- ExFAT drives need the exfat driver: `sudo apt install exfat-fuse`

**Upload fails or stalls:**
- Check available disk space: `df -h /mnt/nas`
- Check for stale temp files: `du -sh /mnt/nas/.uploads/`
- Restart Cove: `sudo systemctl restart cove`

**Videos buffer a lot:**
- Make sure ffmpeg is installed: `which ffmpeg`
- If not: `sudo apt install ffmpeg`
- Run the batch fix command above for existing files

---

## Hardware recommendations

| Use case | Recommendation |
|----------|---------------|
| Light use (photos, docs) | Pi 4 4GB + any USB drive |
| Heavy use (4K video, large library) | Pi 5 8GB + USB SSD |
| Drive format | exFAT (cross-platform) or ext4 (best performance) |
| SD card | 32GB+ Class 10 — only OS lives here, files go on the drive |

An SSD will be noticeably faster than a spinning hard drive for thumbnail loading and video streaming. A Samsung T7 or similar USB SSD is a solid choice.

---

## Built with

- [Go](https://go.dev) — single binary, no runtime dependencies
- [chi](https://github.com/go-chi/chi) — router
- Vanilla HTML/JS — no framework, no build step
- [Tailscale](https://tailscale.com) — secure remote access
- [ffmpeg](https://ffmpeg.org) — video optimization (optional)

---

## License

MIT — do whatever you want with it.
