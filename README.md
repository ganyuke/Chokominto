# Chokominto

A self-hosted place to keep your listening history. Your scrobblers send listens to it the same way they would to ListenBrainz, and you can see your history on its website or in Pano Scrobbler.

It runs as a single program with no other services to set up, and is made to run on a Raspberry Pi.

## Install on a Raspberry Pi

You need a Pi running a 64-bit OS, like Raspberry Pi OS (64-bit).

1. On any computer with Go installed, build the Pi version:

   ```sh
   make pi
   ```

   This creates `dist/chokominto-linux-arm64`. Copy it to the Pi.

2. On the Pi, install it and create an account for it to run as:

   ```sh
   sudo install -m 755 chokominto-linux-arm64 /usr/local/bin/chokominto
   sudo useradd --system --home-dir /var/lib/chokominto --shell /usr/sbin/nologin chokominto
   sudo install -d -m 700 -o chokominto -g chokominto /var/lib/chokominto
   ```

3. Add the settings file and the service:

   ```sh
   sudo install -d /etc/chokominto
   sudo install -m 640 -g chokominto deploy/config.example.toml /etc/chokominto/config.toml
   sudo install -m 644 deploy/chokominto.service /etc/systemd/system/chokominto.service
   ```

   Open `/etc/chokominto/config.toml` and change anything you need. Every setting is explained in the file.

4. Start it:

   ```sh
   sudo systemctl daemon-reload
   sudo systemctl enable --now chokominto
   ```

   Check that it's running with `systemctl status chokominto`. Messages go to `journalctl -u chokominto`.

### Reaching it from outside

Chokominto listens on port 3939 on the Pi itself. To use it from your phone and other computers, put a reverse proxy with HTTPS in front of it. With [Caddy](https://caddyserver.com), the whole setup is:

```
music.example.org {
	reverse_proxy 127.0.0.1:3939
}
```

Then set `public_url = "https://music.example.org"` in the settings file and restart with `sudo systemctl restart chokominto`. From then on, log in through that address. Logging in over plain http on your home network won't keep you logged in.

To try it out on your home network first, set `listen = "0.0.0.0:3939"` instead and open `http://<your Pi's address>:3939`.

## Log in

The first time Chokominto starts, it makes your account, called ruby, with a password made up for you. Find the password with:

```sh
journalctl -u chokominto | grep password
```

Open Chokominto in your browser and log in. You can change the password in Settings, and the name your pages show under Name there. You keep logging in as ruby.

To choose the password yourself, set `CHOKOMINTO_PASSWORD` for the first start, for example in Docker or with `Environment=` in the service file. It's only used when the account is made.

Always run `chokominto` commands as the chokominto account (with `sudo -u chokominto`), so the service can still read and write everything afterwards.

## Connect your scrobblers

Give each scrobbler its own token, so you can stop one without touching the others. Create tokens on the Settings page, or with:

```sh
sudo -u chokominto chokominto token add yourname "Pano Scrobbler"
```

A token is only shown once. Paste it into the scrobbler right away.

The Settings page also lists the addresses below, filled in with yours.

**Pano Scrobbler (Android):** add a ListenBrainz account, choose a custom server, and enter your address with a slash at the end, like `https://music.example.org/`. Paste the token. Your recent listens then show up in Pano too.

**Web Scrobbler (browser):** in its settings under ListenBrainz, set the API URL to your address followed by `/1/submit-listens`, like `https://music.example.org/1/submit-listens`, and paste the token.

Don't use Web Scrobbler's love button with this account. Web Scrobbler always sends loves to listenbrainz.org, along with your token.

If a scrobbler is lost or you stop using it, revoke its token on the Settings page.

## Switch over from Maloja

You can bring your whole Maloja history with you, and keep the keys your scrobblers already use.

1. Stop Maloja, or make a copy of its data folder, so nothing changes while you import.
2. Find two files in Maloja's data folder: `malojadb.sqlite` and `apikeys.yml`.
3. Import them:

   ```sh
   sudo -u chokominto chokominto import maloja --db /path/to/malojadb.sqlite --apikeys /path/to/apikeys.yml
   ```

   The chokominto account needs to be able to read both files. Copying them to `/tmp` first is the easiest way.

4. Change the server address in your scrobblers from Maloja's to Chokominto's. If they already used Maloja's ListenBrainz address, like `https://music.example.org/apis/listenbrainz/`, that same path works on Chokominto, so only the host changes.

Chokominto keeps what your scrobblers originally sent wherever Maloja saved it. For older listens where Maloja only kept its own cleaned-up version, that's used instead, and the import tells you how many.

Running the import again is safe. Listens already in Chokominto are skipped, so you can import once to try it out and again right before you switch.

## Fixing names and links

Every listen has a Fix link when you're logged in. It shows exactly what your scrobbler sent and lets you link it to another song, split it off as a new song, or delete it. After linking, it offers to remember the choice for similar listens.

Review lists songs, artists and albums that look like duplicates, biggest first. Tick several and answer them in one go. Artist, song and album pages have an Edit tab for names, labels, credits and merging.

Settings lists every way Chokominto reads what your scrobblers send, with an example for each: splitting artist lists, character credits, titles in two languages, versions like instrumentals. Turn any of them on or off, and your listens are read again in the background, except the ones you linked by hand. Your own remembered rules are listed there too.

Every change can be undone from the notice that confirms it, or later from the Changes page. That includes deleted listens (for example ones deleted from Pano Scrobbler).

### Album covers and artist pictures

Chokominto looks for album covers and artist pictures on its own, in the background, on iTunes, Deezer and Cover Art Archive. That sends your album and artist names to those sites. When it isn't sure a picture fits, the album's or artist's page shows the ones it found so you can pick, and you can always upload your own. A picture you picked or uploaded is never replaced. Turn looking online off under Pictures in Settings, where you can also remove pictures nothing shows anymore.

After updating to a version with pictures, Chokominto looks for pictures for everything you have, a little at a time. With a few thousand albums that takes a few hours, and everything else keeps working meanwhile.

### Names from MusicBrainz

The Edit tab on artist, song and album pages has a "Get names from MusicBrainz" button. Nothing is asked of MusicBrainz until you press it. MusicBrainz asks every app to say who's calling, so set `public_url` (see [Reaching it from outside](#reaching-it-from-outside)) before using it. Without it, MusicBrainz may answer slowly or not at all.

### Let an AI agent help

An AI agent like Claude Code can help you tidy up: find every spelling of a song, merge the duplicates, name the versions, and link what isn't linked yet. It sees your songs, artists, albums, what your scrobblers sent and the Review list. Every change it makes shows up on the Changes page, marked with the agent's name, and can be undone there.

In Settings, under AI agents, create a token for the agent. Choose whether it can change things or only look. Settings then shows the command for Claude Code, and the address and token for other agents. Revoke the token there to cut the agent off.

If the agent runs where Chokominto does, or can reach it with ssh or `docker exec`, it can also start Chokominto's helper directly, with no token:

```sh
claude mcp add chokominto -- ssh pi sudo -n -u chokominto chokominto mcp -config /etc/chokominto/config.toml
claude mcp add chokominto -- docker exec -i chokominto chokominto mcp
```

Replace `pi` with how you reach the Pi with ssh, and allow your Pi user to run commands as the chokominto account without a password. To let the agent look but not change anything, add `-read-only` at the end.

## Updating

Build and copy the new version the same way as when installing, then:

```sh
sudo install -m 755 chokominto-linux-arm64 /usr/local/bin/chokominto
sudo systemctl restart chokominto
```

If an update needs to change how your data is stored, Chokominto saves a backup first and then updates your data before it starts. With a few years of history that can take a minute or two, and `journalctl -u chokominto` says when it's done. Your scrobblers keep the listens they couldn't send and try again on their own.

After updating to a version with rankings, or after importing from Maloja, Chokominto goes through your history in the background to sort it into songs, artists and albums. Everything works in the meantime. Rankings fill in as it goes, and the site shows how much is left.

After updating to a version with Review, it also looks through your songs, artists and albums for likely duplicates in the background, so Review fills in a minute or so after the update.

## Backups

Chokominto saves a backup of everything once a day into `/var/lib/chokominto/backups` and keeps the last 14. To save one right now:

```sh
sudo -u chokominto chokominto backup
```

Copy that folder somewhere else from time to time, so a failed SD card doesn't take your history with it. Running Chokominto from a USB SSD instead of the SD card also helps a lot.

Chokominto also saves one before each update that changes how your data is stored, named after that version, like `chokominto-before-v4.db`.

To restore a backup, first see which ones there are:

```sh
sudo ls /var/lib/chokominto/backups
```

Then put the one you want back, here the one from 30 September:

```sh
sudo systemctl stop chokominto
sudo -u chokominto rm -f /var/lib/chokominto/chokominto.db-wal /var/lib/chokominto/chokominto.db-shm
sudo -u chokominto cp /var/lib/chokominto/backups/chokominto-2026-09-30.db /var/lib/chokominto/chokominto.db
sudo systemctl start chokominto
```

If the backup is from before an update, start the version it came from, or start the new one and it updates the backup again.

## Commands

```
chokominto serve                          Run the server
chokominto user add <name>                Add another account
chokominto user passwd <name>             Give an account a new password
chokominto token add <user> <label>       Create a scrobbler token
chokominto token list <user>              List scrobbler tokens
chokominto token revoke <user> <id>       Stop a token from working
chokominto import maloja --db <file>      Import your Maloja history
chokominto backup                         Save a backup now
chokominto mcp [-read-only]               Let an AI agent help tidy your music
chokominto version                        Show the version
```

Every command reads `/etc/chokominto/config.toml`. Use `-config <file>` to read a different one.

## For developers

Screenshots of every page with demo listens: `dev/shots/run.sh "/history" "/album/1"` (needs podman. The browser image is built from `dev/shots/Containerfile` the first time). They go to a `chokominto-screenshots` folder next to the repo, and the demo runs without network access.

The design lives in [docs/architecture.md](docs/architecture.md) and the look in [docs/style.md](docs/style.md). Run the tests with `make test`.

To try an install or update without a Pi, `dev/pi-sim/start.sh` boots an emulated one with podman and says how to use it.
