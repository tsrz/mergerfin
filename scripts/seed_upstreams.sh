#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT_DIR"

UPSTREAM_A_PORT="${UPSTREAM_A_PORT:-18097}"
UPSTREAM_B_PORT="${UPSTREAM_B_PORT:-18098}"
UPSTREAM_A_URL="http://127.0.0.1:${UPSTREAM_A_PORT}"
UPSTREAM_B_URL="http://127.0.0.1:${UPSTREAM_B_PORT}"
UPSTREAM_A_USER="admin"
UPSTREAM_A_PASS="upstream-pass-a"
UPSTREAM_B_USER="admin"
UPSTREAM_B_PASS="upstream-pass-b"

CLIENT_HEADER='MediaBrowser Client="Seeder", Device="CLI", DeviceId="seeder-1", Version="12.4.0"'

log() {
    printf '[seed] %s\n' "$*"
}

run_ffmpeg() {
    if command -v ffmpeg >/dev/null 2>&1; then
        ffmpeg "$@"
    else
        docker run --rm \
            -u "$(id -u):$(id -g)" \
            -v "$ROOT_DIR:/work" \
            -w /work \
            --entrypoint /usr/lib/jellyfin-ffmpeg/ffmpeg \
            jellyfin/jellyfin:12.4 "$@"
    fi
}

make_video() {
    local relpath="$1"
    local size="$2"
    mkdir -p "$(dirname "$relpath")"
    if [ -s "$relpath" ]; then
        return 0
    fi
    log "Generating video $relpath ($size)"
    run_ffmpeg -y -hide_banner -loglevel error \
        -f lavfi -i "testsrc2=size=${size}:rate=24" \
        -f lavfi -i "sine=frequency=440:sample_rate=48000" \
        -t 5 \
        -c:v libx264 -preset ultrafast -crf 28 -pix_fmt yuv420p \
        -c:a aac -b:a 128k \
        -movflags +faststart \
        "$relpath"
}

make_image() {
    local relpath="$1"
    local size="$2"
    mkdir -p "$(dirname "$relpath")"
    if [ -s "$relpath" ]; then
        return 0
    fi
    log "Generating image $relpath ($size)"
    run_ffmpeg -y -hide_banner -loglevel error \
        -f lavfi -i "testsrc2=size=${size}:rate=1" \
        -frames:v 1 -q:v 2 \
        "$relpath"
}

write_file_if_missing() {
    local relpath="$1"
    mkdir -p "$(dirname "$relpath")"
    cat > "$relpath"
}

generate_media() {
    log "Generating deterministic test media under test-media/..."

    # Upstream A: Interstellar (2014) 1080p (shared movie)
    local a_interstellar="test-media/upstream-a/movies/Interstellar (2014)"
    make_video "$a_interstellar/Interstellar (2014).mp4" "1920x1080"
    make_image "$a_interstellar/poster.jpg" "600x900"
    make_image "$a_interstellar/backdrop.jpg" "1920x1080"
    write_file_if_missing "$a_interstellar/movie.nfo" <<'EOF'
<?xml version="1.0" encoding="utf-8" standalone="yes"?>
<movie>
  <title>Interstellar</title>
  <originaltitle>Interstellar</originaltitle>
  <year>2014</year>
  <premiered>2014-11-05</premiered>
  <plot>The adventures of a group of explorers who make use of a newly discovered wormhole to surpass the limitations on human space travel.</plot>
  <tmdbid>157336</tmdbid>
  <imdbid>tt0816692</imdbid>
  <uniqueid type="tmdb" default="true">157336</uniqueid>
  <uniqueid type="imdb">tt0816692</uniqueid>
</movie>
EOF

    # Upstream B: Interstellar (2014) 720p (shared movie)
    local b_interstellar="test-media/upstream-b/movies/Interstellar (2014)"
    make_video "$b_interstellar/Interstellar (2014).mp4" "1280x720"
    make_image "$b_interstellar/poster.jpg" "600x900"
    make_image "$b_interstellar/backdrop.jpg" "1920x1080"
    write_file_if_missing "$b_interstellar/movie.nfo" <<'EOF'
<?xml version="1.0" encoding="utf-8" standalone="yes"?>
<movie>
  <title>Interstellar</title>
  <originaltitle>Interstellar</originaltitle>
  <year>2014</year>
  <premiered>2014-11-05</premiered>
  <plot>The adventures of a group of explorers who make use of a newly discovered wormhole to surpass the limitations on human space travel.</plot>
  <tmdbid>157336</tmdbid>
  <imdbid>tt0816692</imdbid>
  <uniqueid type="tmdb" default="true">157336</uniqueid>
  <uniqueid type="imdb">tt0816692</uniqueid>
</movie>
EOF

    # Upstream A: Arrival (2016) 1080p (exclusive to A)
    local a_arrival="test-media/upstream-a/movies/Arrival (2016)"
    make_video "$a_arrival/Arrival (2016).mp4" "1920x1080"
    make_image "$a_arrival/poster.jpg" "600x900"
    write_file_if_missing "$a_arrival/movie.nfo" <<'EOF'
<?xml version="1.0" encoding="utf-8" standalone="yes"?>
<movie>
  <title>Arrival</title>
  <originaltitle>Arrival</originaltitle>
  <year>2016</year>
  <premiered>2016-11-10</premiered>
  <plot>Taking place after alien crafts land around the world, an expert linguist is recruited by the military to determine whether they come in peace or are a threat.</plot>
  <tmdbid>329865</tmdbid>
  <uniqueid type="tmdb" default="true">329865</uniqueid>
</movie>
EOF

    # Shared TV Series: Severance (S01E01 on A, S01E02 on B)
    local a_sev="test-media/upstream-a/tvshows/Severance"
    make_image "$a_sev/poster.jpg" "600x900"
    write_file_if_missing "$a_sev/tvshow.nfo" <<'EOF'
<?xml version="1.0" encoding="utf-8" standalone="yes"?>
<tvshow>
  <title>Severance</title>
  <year>2022</year>
  <premiered>2022-02-18</premiered>
  <plot>Mark leads a team of office workers whose memories have been surgically divided between their work and personal lives.</plot>
  <tmdbid>95396</tmdbid>
  <tvdbid>371980</tvdbid>
  <uniqueid type="tmdb" default="true">95396</uniqueid>
  <uniqueid type="tvdb">371980</uniqueid>
</tvshow>
EOF
    make_video "$a_sev/Season 01/Severance - S01E01.mp4" "1920x1080"
    write_file_if_missing "$a_sev/Season 01/Severance - S01E01.nfo" <<'EOF'
<?xml version="1.0" encoding="utf-8" standalone="yes"?>
<episodedetails>
  <title>Good News About Hell</title>
  <season>1</season>
  <episode>1</episode>
  <plot>Mark is promoted to lead a team of employees who have had their memories surgically divided.</plot>
</episodedetails>
EOF

    local b_sev="test-media/upstream-b/tvshows/Severance"
    make_image "$b_sev/poster.jpg" "600x900"
    write_file_if_missing "$b_sev/tvshow.nfo" <<'EOF'
<?xml version="1.0" encoding="utf-8" standalone="yes"?>
<tvshow>
  <title>Severance</title>
  <year>2022</year>
  <premiered>2022-02-18</premiered>
  <plot>Mark leads a team of office workers whose memories have been surgically divided between their work and personal lives.</plot>
  <tmdbid>95396</tmdbid>
  <tvdbid>371980</tvdbid>
  <uniqueid type="tmdb" default="true">95396</uniqueid>
  <uniqueid type="tvdb">371980</uniqueid>
</tvshow>
EOF
    make_video "$b_sev/Season 01/Severance - S01E02.mp4" "1280x720"
    write_file_if_missing "$b_sev/Season 01/Severance - S01E02.nfo" <<'EOF'
<?xml version="1.0" encoding="utf-8" standalone="yes"?>
<episodedetails>
  <title>Half Loop</title>
  <season>1</season>
  <episode>2</episode>
  <plot>The team trains new hire Helly on macrodata refinement.</plot>
</episodedetails>
EOF
}

json_get() {
    local expr="$1"
    python3 -c "import json, sys; data = json.load(sys.stdin); print($expr)"
}

wait_for_jellyfin() {
    local url="$1"
    local name="$2"
    local attempts=0
    local max_attempts=60

    log "Waiting for $name at $url ..."
    while [ "$attempts" -lt "$max_attempts" ]; do
        if curl -fsS "$url/System/Info/Public" >/dev/null 2>&1; then
            log "$name is responding."
            return 0
        fi
        attempts=$((attempts + 1))
        sleep 2
    done
    printf '[seed] ERROR: %s did not become ready at %s\n' "$name" "$url" >&2
    return 1
}

configure_upstream() {
    local url="$1"
    local name="$2"
    local user="$3"
    local pass="$4"
    local expected_items="$5"

    wait_for_jellyfin "$url" "$name"

    local pub_info wizard_done
    pub_info="$(curl -fsS "$url/System/Info/Public")"
    wizard_done="$(printf '%s' "$pub_info" | json_get "str(data.get('StartupWizardCompleted', False)).lower()")"

    if [ "$wizard_done" != "true" ]; then
        log "Running Jellyfin startup wizard on $name..."
        curl -fsS -X POST "$url/Startup/Configuration" \
            -H 'Content-Type: application/json' \
            -d '{"UICulture":"en-US","MetadataCountryCode":"US","PreferredMetadataLanguage":"en"}' >/dev/null

        curl -fsS "$url/Startup/User" >/dev/null

        local user_payload
        user_payload="$(python3 -c 'import json, sys; print(json.dumps({"Name": sys.argv[1], "Password": sys.argv[2]}))' "$user" "$pass")"
        curl -fsS -X POST "$url/Startup/User" \
            -H 'Content-Type: application/json' \
            -d "$user_payload" >/dev/null

        curl -fsS -X POST "$url/Startup/RemoteAccess" \
            -H 'Content-Type: application/json' \
            -d '{"EnableRemoteAccess":true,"EnableAutomaticPortMapping":false}' >/dev/null

        curl -fsS -X POST "$url/Startup/Complete" >/dev/null
        log "Startup wizard completed on $name."
    else
        log "Startup wizard already completed on $name."
    fi

    log "Authenticating to $name as $user..."
    local auth_payload auth_resp token user_id auth_header
    auth_payload="$(python3 -c 'import json, sys; print(json.dumps({"Username": sys.argv[1], "Pw": sys.argv[2]}))' "$user" "$pass")"
    auth_resp="$(curl -fsS -X POST "$url/Users/AuthenticateByName" \
        -H 'Content-Type: application/json' \
        -H "Authorization: $CLIENT_HEADER" \
        -d "$auth_payload")"

    token="$(printf '%s' "$auth_resp" | json_get "data['AccessToken']")"
    user_id="$(printf '%s' "$auth_resp" | json_get "data['User']['Id']")"
    auth_header="${CLIENT_HEADER}, Token=\"${token}\""

    local folders has_movies has_tv
    folders="$(curl -fsS "$url/Library/VirtualFolders" -H "Authorization: $auth_header")"
    has_movies="$(printf '%s' "$folders" | json_get "any(f.get('Name') == 'Movies' for f in data)")"
    has_tv="$(printf '%s' "$folders" | json_get "any(f.get('Name') == 'TV Shows' for f in data)")"

    if [ "$has_movies" != "True" ]; then
        log "Creating Movies virtual folder on $name..."
        curl -fsS -X POST "$url/Library/VirtualFolders?name=Movies&collectionType=movies&paths=%2Fmedia%2Fmovies&refreshLibrary=false" \
            -H 'Content-Type: application/json' \
            -H "Authorization: $auth_header" \
            -d '{
  "LibraryOptions": {
    "EnablePhotos": false,
    "EnableRealtimeMonitor": false,
    "EnableInternetProviders": false,
    "SaveLocalMetadata": false,
    "LocalMetadataReaderOrder": ["Nfo"],
    "DisabledLocalMetadataReaders": [],
    "TypeOptions": [
      {
        "Type": "Movie",
        "MetadataFetchers": [],
        "MetadataFetcherOrder": [],
        "ImageFetchers": [],
        "ImageFetcherOrder": []
      }
    ]
  }
}' >/dev/null
    else
        log "Movies virtual folder already exists on $name."
    fi

    if [ "$has_tv" != "True" ]; then
        log "Creating TV Shows virtual folder on $name..."
        curl -fsS -X POST "$url/Library/VirtualFolders?name=TV%20Shows&collectionType=tvshows&paths=%2Fmedia%2Ftvshows&refreshLibrary=false" \
            -H 'Content-Type: application/json' \
            -H "Authorization: $auth_header" \
            -d '{
  "LibraryOptions": {
    "EnablePhotos": false,
    "EnableRealtimeMonitor": false,
    "EnableInternetProviders": false,
    "SaveLocalMetadata": false,
    "LocalMetadataReaderOrder": ["Nfo"],
    "DisabledLocalMetadataReaders": [],
    "TypeOptions": [
      {
        "Type": "Series",
        "MetadataFetchers": [],
        "MetadataFetcherOrder": [],
        "ImageFetchers": [],
        "ImageFetcherOrder": []
      },
      {
        "Type": "Season",
        "MetadataFetchers": [],
        "MetadataFetcherOrder": [],
        "ImageFetchers": [],
        "ImageFetcherOrder": []
      },
      {
        "Type": "Episode",
        "MetadataFetchers": [],
        "MetadataFetcherOrder": [],
        "ImageFetchers": [],
        "ImageFetcherOrder": []
      }
    ]
  }
}' >/dev/null
    else
        log "TV Shows virtual folder already exists on $name."
    fi

    log "Triggering library refresh on $name..."
    curl -fsS -X POST "$url/Library/Refresh" -H "Authorization: $auth_header" >/dev/null

    log "Polling $name for $expected_items indexed items (Movie,Episode)..."
    local attempts=0
    local max_attempts=60
    local count=0
    while [ "$attempts" -lt "$max_attempts" ]; do
        local items_resp
        items_resp="$(curl -fsS "$url/Users/$user_id/Items?Recursive=true&IncludeItemTypes=Movie,Episode" -H "Authorization: $auth_header")"
        count="$(printf '%s' "$items_resp" | json_get "int(data.get('TotalRecordCount', 0))")"
        if [ "$count" -ge "$expected_items" ]; then
            log "$name indexed $count/$expected_items items."
            return 0
        fi
        attempts=$((attempts + 1))
        sleep 2
    done

    printf '[seed] ERROR: %s indexed %s items, expected >= %s\n' "$name" "$count" "$expected_items" >&2
    return 1
}

main() {
    generate_media
    configure_upstream "$UPSTREAM_A_URL" "Upstream A" "$UPSTREAM_A_USER" "$UPSTREAM_A_PASS" 3
    configure_upstream "$UPSTREAM_B_URL" "Upstream B" "$UPSTREAM_B_USER" "$UPSTREAM_B_PASS" 2
    log "Seeding complete!"
}

main "$@"
