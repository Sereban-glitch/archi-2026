#!/usr/bin/env bash
# ============================================================
# Archi 2026.16 «Push-To-Talk» — setup-hotkey.sh
# Привязка archi ptt к глобальному хоткею Cinnamon (Linux Mint).
#
# По умолчанию: Super+A («A» — Archi) → /usr/local/bin/archi ptt
#
# Использование:
#   bash deploy/setup-hotkey.sh                     # Super+A → archi ptt
#   bash deploy/setup-hotkey.sh Super+V             # другой хоткей
#   bash deploy/setup-hotkey.sh Super+A -- ptt      # аргументы archi после «--»
#   bash deploy/setup-hotkey.sh --status            # показать custom-биндинги
#   bash deploy/setup-hotkey.sh --remove            # снять привязку Archi Voice
#
# ВЫБОР КЛАВИШ (проверено владельцем на Mint 22 Cinnamon/X11):
#   ЗАНЯТЫ:  Ctrl+Alt (voice_typer: локальный и облачный набор), Ctrl+Shift+E
#            (перевод экрана). AltGr = правый Ctrl+Alt — тоже занят.
#   СВОБОДНЫ: Super+A (рекомендуем: мнемоника «А» — Арчи), Super+V,
#            Super+Space, Ctrl+Shift+Space.
#
# Механика: gsettings → org.cinnamon.desktop.keybindings custom-list +
# relocatable-схема custom-keybinding:/org/cinnamon/desktop/keybindings/
# custom-keybindings/customN/{name,binding,command}. Повторный запуск
# обновляет существующую привязку «Archi Voice» (идемпотентно).
# ============================================================
set -euo pipefail

SCHEMA_ROOT="org.cinnamon.desktop.keybindings"
SCHEMA_KB="org.cinnamon.desktop.keybindings.custom-keybinding"
KB_ROOT="/org/cinnamon/desktop/keybindings/custom-keybindings"
BINDING_NAME="Archi Voice"
DEFAULT_BINDING="Super+A"

die() { echo "ОШИБКА: $*" >&2; exit 1; }

require_gsettings() {
    command -v gsettings >/dev/null 2>&1 \
        || die "gsettings не найден — нужен рабочий стол Cinnamon (Linux Mint)"
}

# normalize_binding: Super+A | <Super>a | super+a → <Super>a (формат GSettings)
normalize_binding() {
    local raw="${1:-$DEFAULT_BINDING}"
    raw="${raw#<}"; raw="${raw%>}"
    local mods="" key="" part low
    local IFS='+'
    read -ra parts <<< "$raw"
    for part in "${parts[@]}"; do
        low="$(printf '%s' "$part" | tr '[:upper:]' '[:lower:]')"
        case "$low" in
            super|win|meta) mods+="<Super>" ;;
            ctrl|control)    mods+="<Control>" ;;
            shift)           mods+="<Shift>" ;;
            alt)             mods+="<Alt>" ;;
            space)           key="space" ;;
            '')              ;;
            *)               key="$low" ;;
        esac
    done
    [ -n "$key" ] || die "не разобрал биндинг «$1» (пример: Super+A)"
    printf '%s%s' "$mods" "$key"
}

# current_ids: 'custom0' 'custom1' … из custom-list (пусто, если @as []).
current_ids() {
    gsettings get "$SCHEMA_ROOT" custom-list 2>/dev/null \
        | grep -o 'custom[0-9]\+' || true
}

# kb_get <id> <key>: значение relocatable-ключа без обрамляющих кавычек.
kb_get() {
    local v
    v="$(gsettings get "$SCHEMA_KB:$KB_ROOT/$1/" "$2" 2>/dev/null || true)"
    v="${v#\'}"; v="${v%\'}"
    printf '%s' "$v"
}

# find_existing: id слота с именем Archi Voice (или пусто).
find_existing() {
    local id
    while read -r id; do
        [ -n "$id" ] || continue
        if [ "$(kb_get "$id" name)" = "$BINDING_NAME" ]; then
            echo "$id"
            return 0
        fi
    done < <(current_ids)
    return 0
}

# free_slot: минимальный незанятый customN.
free_slot() {
    local n=0 existing id
    existing="$(current_ids)"
    while true; do
        id="custom$n"
        if ! printf '%s\n' "$existing" | grep -qx "$id"; then
            echo "$id"
            return 0
        fi
        n=$((n + 1))
    done
}

# join_ids <include-id>|<exclude-id>: массив id → "['custom0', 'custom2']".
# Второй аргумент непустой → этот id выкидывается (для --remove).
join_ids() {
    local include="$1" exclude="${2:-}" id out="[" first=1
    while read -r id; do
        [ -n "$id" ] || continue
        [ "$id" = "$exclude" ] && continue
        if [ "$first" = 1 ]; then out+="'$id'"; first=0
        else out+=", '$id'"; fi
    done < <(current_ids)
    if [ "$include" != "" ] && [ "$include" != "$exclude" ]; then
        if printf '%s' "$out" | grep -q "'$include'"; then :
        else
            if [ "$first" = 1 ]; then out+="'$include'"; first=0
            else out+=", '$include'"; fi
        fi
    fi
    out+="]"
    if [ "$out" = "[]" ]; then out="@as []"; fi
    printf '%s' "$out"
}

# resolve_archi: где лежит бинарь (make install → /usr/local/bin/archi).
resolve_archi() {
    if [ -x /usr/local/bin/archi ]; then
        echo "/usr/local/bin/archi"; return 0
    fi
    if command -v archi >/dev/null 2>&1; then
        command -v archi; return 0
    fi
    local repo_bin
    repo_bin="$(cd "$(dirname "$0")/.." && pwd)/bin/archi"
    if [ -x "$repo_bin" ]; then
        echo "$repo_bin"; return 0
    fi
    return 1
}

show_status() {
    require_gsettings
    local id found=0
    echo "Custom keybindings Cinnamon:"
    while read -r id; do
        [ -n "$id" ] || continue
        found=1
        printf '  %-10s %-30s [%s] → %s\n' \
            "$id" "$(kb_get "$id" name)" "$(kb_get "$id" binding)" "$(kb_get "$id" command)"
    done < <(current_ids)
    [ "$found" = 1 ] || echo "  (пусто — привязок нет)"
}

do_remove() {
    require_gsettings
    local id
    id="$(find_existing)"
    if [ -z "$id" ]; then
        echo "Привязка «$BINDING_NAME» не найдена — чисто."
        exit 0
    fi
    gsettings set "$SCHEMA_ROOT" custom-list "$(join_ids "" "$id")"
    gsettings reset "$SCHEMA_KB:$KB_ROOT/$id/" name 2>/dev/null || true
    gsettings reset "$SCHEMA_KB:$KB_ROOT/$id/" binding 2>/dev/null || true
    gsettings reset "$SCHEMA_KB:$KB_ROOT/$id/" command 2>/dev/null || true
    echo "Снята привязка «$BINDING_NAME» (слот $id)."
}

do_setup() {
    local binding_raw="$1"; shift || true
    local archi_args=("$@")
    if [ "${#archi_args[@]}" -eq 0 ]; then
        archi_args=(ptt)
    fi

    require_gsettings

    local archi_bin
    archi_bin="$(resolve_archi)" \
        || die "бинарь archi не найден. Сделай: make install (→ /usr/local/bin/archi)"

    local binding id command_str
    binding="$(normalize_binding "$binding_raw")"
    id="$(find_existing)"
    if [ -n "$id" ]; then
        echo "Обновляю существующую привязку «$BINDING_NAME» (слот $id)…"
    else
        id="$(free_slot)"
        echo "Создаю привязку «$BINDING_NAME» (слот $id)…"
    fi

    command_str="$archi_bin ${archi_args[*]}"

    # Слоты Cinnamon: gsettings-строки требуют двойного обрамления.
    gsettings set "$SCHEMA_ROOT" custom-list "$(join_ids "$id" "")"
    gsettings set "$SCHEMA_KB:$KB_ROOT/$id/" name "'$BINDING_NAME'"
    gsettings set "$SCHEMA_KB:$KB_ROOT/$id/" binding "['$binding']"
    gsettings set "$SCHEMA_KB:$KB_ROOT/$id/" command "'$command_str'"

    echo
    echo "Готово:"
    echo "  хоткей:   $binding_raw → $binding"
    echo "  команда:  $command_str"
    echo
    echo "Проверка: нажми $binding_raw → мягкий бип → скажи фразу → замолчи."
    echo "Отладка в терминале: archi ptt --no-beep"
    echo "Снять: bash deploy/setup-hotkey.sh --remove"
}

# ---- разбор аргументов ----
case "${1:-}" in
    --status) show_status; exit 0 ;;
    --remove) do_remove; exit 0 ;;
    -h|--help) sed -n '2,30p' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
esac

BINDING_RAW="${1:-$DEFAULT_BINDING}"
[ "${1:-}" != "--" ] || BINDING_RAW="$DEFAULT_BINDING"
shift || true
# всё после «--» (или после биндинга) — аргументы archi (по умолчанию ptt)
if [ "${1:-}" = "--" ]; then shift; fi

do_setup "$BINDING_RAW" "$@"
