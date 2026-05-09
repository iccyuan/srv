# Dracula colour theme for ls/grep -- 256-colour LS_COLORS palette
# derived from https://draculatheme.com:
#   purple dirs, cyan links, green executables,
#   pink media, red archives, yellow data files,
#   cyan source code.
#
# Loaded by srv as the built-in fallback when neither the caller's
# local shell nor the remote env provides a non-empty LS_COLORS.
# Drop a copy into ~/.srv/init/<name>.sh and `srv color use <name>`
# to use it as a named preset (or to start from when customising).
[ -n "$LS_COLORS" ] || export LS_COLORS='no=00:fi=00:di=01;38;5;141:ln=01;38;5;117:pi=38;5;228:so=01;38;5;212:bd=38;5;228;01:cd=38;5;228;01:or=01;38;5;203:mi=01;38;5;203:ex=01;38;5;120:*.tar=38;5;203:*.tgz=38;5;203:*.gz=38;5;203:*.bz2=38;5;203:*.xz=38;5;203:*.zst=38;5;203:*.zip=38;5;203:*.7z=38;5;203:*.rar=38;5;203:*.rpm=38;5;203:*.deb=38;5;203:*.iso=38;5;203:*.jpg=38;5;212:*.jpeg=38;5;212:*.png=38;5;212:*.gif=38;5;212:*.bmp=38;5;212:*.svg=38;5;212:*.ico=38;5;212:*.webp=38;5;212:*.mp3=38;5;212:*.mp4=38;5;212:*.mkv=38;5;212:*.avi=38;5;212:*.mov=38;5;212:*.flac=38;5;212:*.wav=38;5;212:*.md=38;5;228:*.txt=38;5;228:*.log=38;5;228:*.json=38;5;228:*.yaml=38;5;228:*.yml=38;5;228:*.toml=38;5;228:*.conf=38;5;228:*.ini=38;5;228:*.csv=38;5;228:*.go=01;38;5;117:*.py=01;38;5;117:*.js=01;38;5;117:*.ts=01;38;5;117:*.tsx=01;38;5;117:*.jsx=01;38;5;117:*.rs=01;38;5;117:*.c=01;38;5;117:*.h=01;38;5;117:*.cpp=01;38;5;117:*.hpp=01;38;5;117:*.java=01;38;5;117:*.kt=01;38;5;117:*.swift=01;38;5;117:*.rb=01;38;5;117:*.php=01;38;5;117:*.sh=38;5;120:*.bash=38;5;120:*.zsh=38;5;120:*.fish=38;5;120'
