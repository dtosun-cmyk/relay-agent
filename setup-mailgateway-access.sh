#!/bin/bash
#
# Mailgateway Access Setup Script
# Kullanım:
#   ./setup-mailgateway-access.sh                     - Mevcut yapılandırmayı göster
#   ./setup-mailgateway-access.sh --create <isim>    - Yeni kullanıcı/database oluştur
#   ./setup-mailgateway-access.sh --list             - Tüm kullanıcıları listele
#   ./setup-mailgateway-access.sh --delete <isim>    - Kullanıcı/database sil
#

set -e

# Colors
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
CYAN='\033[0;36m'
MAGENTA='\033[0;35m'
NC='\033[0m'

# Config paths
CONFIG_FILE="/opt/relay-agent/config/config.yaml"
MONGOD_CONF="/etc/mongod.conf"
CREDENTIALS_FILE="/opt/relay-agent/credentials/install-credentials.txt"

log_info() { echo -e "${GREEN}[INFO]${NC} $1"; }
log_warn() { echo -e "${YELLOW}[WARN]${NC} $1"; }
log_error() { echo -e "${RED}[ERROR]${NC} $1"; }
log_step() { echo -e "${BLUE}[STEP]${NC} $1"; }

# Check root
check_root() {
    if [ "$EUID" -ne 0 ]; then
        log_error "Bu scripti root olarak çalıştırın: sudo $0"
        exit 1
    fi
}

# Read credentials from install-credentials.txt
load_credentials() {
    if [ ! -f "$CREDENTIALS_FILE" ]; then
        log_error "Credentials dosyası bulunamadı: $CREDENTIALS_FILE"
        log_error "Önce install.sh ile kurulum yapılmalıdır."
        exit 1
    fi

    ADMIN_PASS=$(extract_password "$CREDENTIALS_FILE" "MONGODB ADMIN")
    RELAY_AGENT_PASS=$(extract_password "$CREDENTIALS_FILE" "MONGODB RELAY_AGENT")

    if [ -z "$ADMIN_PASS" ]; then
        log_error "Admin şifresi credentials dosyasından okunamadı!"
        exit 1
    fi
    if [ -z "$RELAY_AGENT_PASS" ]; then
        log_error "Relay agent şifresi credentials dosyasından okunamadı!"
        exit 1
    fi
}

# Get server IP
get_server_ip() {
    local ip
    ip=$(ip -4 route get 1.1.1.1 2>/dev/null | awk '/src/ {for(i=1;i<=NF;i++) if($i=="src"){print $(i+1); exit}}')
    if [ -z "$ip" ]; then
        ip=$(hostname -I 2>/dev/null | tr ' ' '\n' | grep -E '^[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+$' | grep -v '^127\.' | head -1)
    fi
    if [ -z "$ip" ]; then
        ip="127.0.0.1"
    fi
    echo "$ip"
}

# Generate random password
generate_password() {
    openssl rand -base64 18 | tr -d '/+=' | head -c 24
}

# URL encode for MongoDB connection strings
# Handles special characters in passwords (@, :, /, etc.)
url_encode() {
    local string="$1"
    local encoded=""
    local pos c o
    
    for (( pos=0; pos<${#string}; pos++ )); do
        c="${string:$pos:1}"
        case "$c" in
            [-_.~a-zA-Z0-9]) encoded+="$c" ;;
            *) printf -v o '%%%02x' "'$c"; encoded+="$o" ;;
        esac
    done
    echo "$encoded"
}

# Build admin MongoDB URI safely
get_admin_mongo_uri() {
    local encoded_admin_pass
    encoded_admin_pass=$(url_encode "${ADMIN_PASS}")
    echo "mongodb://admin:${encoded_admin_pass}@localhost:27017/admin?authSource=admin&replicaSet=rs0&directConnection=true"
}

# Verify admin authentication before mutating operations
verify_admin_auth() {
    local admin_uri
    admin_uri=$(get_admin_mongo_uri)
    if ! mongosh "$admin_uri" --quiet --eval "db.runCommand({ ping: 1 }).ok" 2>/dev/null | grep -q "1"; then
        log_error "MongoDB admin auth basarisiz. credentials/install-credentials.txt ile MongoDB parolasi uyusmuyor olabilir."
        log_error "Cozum: install.sh scriptini tekrar calistirip password sync edin."
        exit 1
    fi
}

# Safe password extraction from credentials file
# Handles passwords with spaces and special characters
extract_password() {
    local file="$1"
    local section="$2"
    local pass_line
    
    pass_line=$(grep -A5 "^${section}:" "$file" 2>/dev/null | grep "^Password:")
    if [ -n "$pass_line" ]; then
        # Extract everything after "Password:" and trim leading whitespace
        echo "${pass_line#Password:}" | sed 's/^[[:space:]]*//'
    fi
}

# Show usage
show_usage() {
    echo ""
    echo -e "${CYAN}Kullanım:${NC}"
    echo "  $0                      Mevcut yapılandırmayı göster"
    echo "  $0 --create <isim>      Yeni kullanıcı/database oluştur"
    echo "  $0 --list               Tüm kullanıcıları listele"
    echo "  $0 --delete <isim>      Kullanıcı/database sil"
    echo "  $0 --help               Bu yardım mesajını göster"
    echo ""
    echo -e "${CYAN}Örnekler:${NC}"
    echo "  $0 --create mailgateway1    # mailgateway1 için database oluştur"
    echo "  $0 --create customer_abc    # customer_abc için database oluştur"
    echo "  $0 --list                   # Tüm kullanıcıları listele"
    echo ""
}

# Create new user with dedicated database
create_user() {
    local USERNAME="$1"

    if [ -z "$USERNAME" ]; then
        log_error "Kullanıcı adı belirtilmedi!"
        echo "Kullanım: $0 --create <kullanıcı_adı>"
        exit 1
    fi

    # Sanitize username (only alphanumeric and underscore)
    USERNAME=$(echo "$USERNAME" | tr -cd '[:alnum:]_' | tr '[:upper:]' '[:lower:]')

    if [ -z "$USERNAME" ]; then
        log_error "Geçersiz kullanıcı adı!"
        exit 1
    fi

    load_credentials
    verify_admin_auth

    SERVER_IP=$(get_server_ip)
    DATABASE="relay_${USERNAME}"
    WRITER_PASSWORD=$(generate_password)
    READER_PASSWORD=$(generate_password)

    log_step "Database ve kullanıcılar oluşturuluyor: $USERNAME"
    log_info "Database: $DATABASE"

    ADMIN_URI=$(get_admin_mongo_uri)

    # Create database, users, and indexes using admin credentials
    mongosh "$ADMIN_URI" --quiet <<EOF
// Switch to user database
use $DATABASE

// Create writer user (for relay-agent)
try {
    db.createUser({
        user: "${USERNAME}_writer",
        pwd: "$WRITER_PASSWORD",
        roles: [
            { role: "readWrite", db: "$DATABASE" }
        ]
    });
    print("Writer user created successfully");
} catch(e) {
    if (e.codeName == "DuplicateKey") {
        db.updateUser("${USERNAME}_writer", { pwd: "$WRITER_PASSWORD" });
        print("Writer user password updated");
    } else {
        print("Error creating writer: " + e.message);
    }
}

// Create reader user (for mailgateway)
try {
    db.createUser({
        user: "${USERNAME}_reader",
        pwd: "$READER_PASSWORD",
        roles: [
            { role: "read", db: "$DATABASE" }
        ]
    });
    print("Reader user created successfully");
} catch(e) {
    if (e.codeName == "DuplicateKey") {
        db.updateUser("${USERNAME}_reader", { pwd: "$READER_PASSWORD" });
        print("Reader user password updated");
    } else {
        print("Error creating reader: " + e.message);
    }
}

// Create comprehensive indexes in emails collection
print("Creating indexes...");

// Unique queue_id index (sparse for documents without queue_id)
db.emails.createIndex(
    { "queue_id": 1 },
    { name: "idx_queue_id", unique: true, sparse: true }
);

// Mailgateway queue_id index (sparse)
db.emails.createIndex(
    { "mailgateway_queue_id": 1 },
    { name: "idx_mailgateway_queue_id", sparse: true }
);

// Compound index for recipient domain queries
db.emails.createIndex(
    { "recipient_domain": 1, "created_at": -1 },
    { name: "idx_recipient_domain_created" }
);

// Compound index for status queries
db.emails.createIndex(
    { "status": 1, "created_at": -1 },
    { name: "idx_status_created" }
);

// Descending created_at index
db.emails.createIndex(
    { "created_at": -1 },
    { name: "idx_created_at" }
);

// TTL index - expires documents after 30 days (2592000 seconds)
db.emails.createIndex(
    { "created_at": 1 },
    { name: "idx_created_at_ttl", expireAfterSeconds: 2592000 }
);

// Webhook status compound index
db.emails.createIndex(
    { "webhook_sent": 1, "status": 1 },
    { name: "idx_webhook_status" }
);

// Sender index
db.emails.createIndex(
    { "sender": 1 },
    { name: "idx_sender" }
);

// Recipient index
db.emails.createIndex(
    { "recipient": 1 },
    { name: "idx_recipient" }
);

print("All indexes created successfully");
EOF

    echo ""
    echo -e "${CYAN}=========================================${NC}"
    echo -e "${CYAN}  YENİ DATABASE VE KULLANICILAR OLUŞTURULDU${NC}"
    echo -e "${CYAN}=========================================${NC}"
    echo ""

    echo -e "${GREEN}▶ Database Bilgileri${NC}"
    echo "  ─────────────────────────────────────"
    echo -e "  Database:       ${YELLOW}${DATABASE}${NC}"
    echo -e "  Collection:     ${YELLOW}emails${NC}"
    echo -e "  Host:           ${YELLOW}${SERVER_IP}${NC}"
    echo -e "  Port:           ${YELLOW}27017${NC}"
    echo -e "  Replica Set:    ${YELLOW}rs0${NC}"
    echo ""

    echo -e "${GREEN}▶ Writer Kullanıcı (Relay-Agent için)${NC}"
    echo "  ─────────────────────────────────────"
    echo -e "  Kullanıcı Adı:  ${YELLOW}${USERNAME}_writer${NC}"
    echo -e "  Şifre:          ${YELLOW}${WRITER_PASSWORD}${NC}"
    echo -e "  Yetki:          ${YELLOW}readWrite${NC}"
    echo ""
    # URL encode passwords for display (to show correct connection strings)
    ENCODED_WRITER_PASS=$(url_encode "${WRITER_PASSWORD}")
    ENCODED_READER_PASS=$(url_encode "${READER_PASSWORD}")
    
    echo -e "  Connection String:"
    echo -e "  ${CYAN}mongodb://${USERNAME}_writer:${ENCODED_WRITER_PASS}@${SERVER_IP}:27017/${DATABASE}?authSource=${DATABASE}&replicaSet=rs0${NC}"
    echo ""

    echo -e "${GREEN}▶ Reader Kullanıcı (Mailgateway için)${NC}"
    echo "  ─────────────────────────────────────"
    echo -e "  Kullanıcı Adı:  ${YELLOW}${USERNAME}_reader${NC}"
    echo -e "  Şifre:          ${YELLOW}${READER_PASSWORD}${NC}"
    echo -e "  Yetki:          ${YELLOW}read${NC}"
    echo ""
    echo -e "  Connection String:"
    echo -e "  ${CYAN}mongodb://${USERNAME}_reader:${ENCODED_READER_PASS}@${SERVER_IP}:27017/${DATABASE}?authSource=${DATABASE}&replicaSet=rs0${NC}"
    echo ""

    echo -e "${GREEN}▶ Oluşturulan Index'ler${NC}"
    echo "  ─────────────────────────────────────"
    echo -e "  ${YELLOW}idx_queue_id${NC}                  (unique, sparse)"
    echo -e "  ${YELLOW}idx_mailgateway_queue_id${NC}      (sparse)"
    echo -e "  ${YELLOW}idx_recipient_domain_created${NC}  (compound)"
    echo -e "  ${YELLOW}idx_status_created${NC}            (compound)"
    echo -e "  ${YELLOW}idx_created_at${NC}                (descending)"
    echo -e "  ${YELLOW}idx_created_at_ttl${NC}            (TTL 30 gün)"
    echo -e "  ${YELLOW}idx_webhook_status${NC}            (compound)"
    echo -e "  ${YELLOW}idx_sender${NC}"
    echo -e "  ${YELLOW}idx_recipient${NC}"
    echo ""

    echo -e "${MAGENTA}▶ Relay-Agent Config Snippet (config.yaml)${NC}"
    echo "  ─────────────────────────────────────"
    cat <<CONFIGSNIPPET
${CYAN}mongodb:
  uri: "mongodb://${USERNAME}_writer:${ENCODED_WRITER_PASS}@${SERVER_IP}:27017/${DATABASE}?authSource=${DATABASE}&replicaSet=rs0"
  database: "${DATABASE}"
  collection: "emails"
  timeout: 10s${NC}
CONFIGSNIPPET
    echo ""

    # Save to credentials file
    CRED_FILE="/opt/relay-agent/credentials/${USERNAME}.txt"
    mkdir -p /opt/relay-agent/credentials
    chmod 700 /opt/relay-agent/credentials

    cat > "$CRED_FILE" <<CREDEOF
# MongoDB Credentials for: $USERNAME
# Created: $(date)

DATABASE INFORMATION:
====================
Database:    ${DATABASE}
Collection:  emails
Host:        ${SERVER_IP}
Port:        27017
Replica Set: rs0

WRITER USER (Relay-Agent):
==========================
Username:    ${USERNAME}_writer
Password:    ${WRITER_PASSWORD}
Permissions: readWrite on ${DATABASE}

Connection String:
mongodb://${USERNAME}_writer:${ENCODED_WRITER_PASS}@${SERVER_IP}:27017/${DATABASE}?authSource=${DATABASE}&replicaSet=rs0

READER USER (Mailgateway):
==========================
Username:    ${USERNAME}_reader
Password:    ${READER_PASSWORD}
Permissions: read on ${DATABASE}

Connection String:
mongodb://${USERNAME}_reader:${ENCODED_READER_PASS}@${SERVER_IP}:27017/${DATABASE}?authSource=${DATABASE}&replicaSet=rs0

RELAY-AGENT CONFIG SNIPPET:
===========================
mongodb:
  uri: "mongodb://${USERNAME}_writer:${WRITER_PASSWORD}@${SERVER_IP}:27017/${DATABASE}?authSource=${DATABASE}&replicaSet=rs0"
  database: "${DATABASE}"
  collection: "emails"
  timeout: 10s

INDEXES:
========
- idx_queue_id (unique, sparse)
- idx_mailgateway_queue_id (sparse)
- idx_recipient_domain_created (compound)
- idx_status_created (compound)
- idx_created_at (descending)
- idx_created_at_ttl (TTL 30 days)
- idx_webhook_status (compound)
- idx_sender
- idx_recipient
CREDEOF
    chmod 600 "$CRED_FILE"

    echo -e "${GREEN}▶ Credentials dosyası kaydedildi:${NC}"
    echo -e "  ${CYAN}${CRED_FILE}${NC}"
    echo ""
}

# List all users
list_users() {
    load_credentials

    SERVER_IP=$(get_server_ip)

    echo ""
    echo -e "${CYAN}=========================================${NC}"
    echo -e "${CYAN}  KAYITLI KULLANICILAR${NC}"
    echo -e "${CYAN}=========================================${NC}"
    echo ""

    # List from credentials directory
    if [ -d /opt/relay-agent/credentials ] && [ "$(ls -A /opt/relay-agent/credentials/*.txt 2>/dev/null | grep -v install-credentials.txt)" ]; then
        echo -e "${GREEN}▶ Oluşturulan Kullanıcılar:${NC}"
        echo "  ─────────────────────────────────────"

        for cred_file in /opt/relay-agent/credentials/*.txt; do
            if [ -f "$cred_file" ] && [ "$(basename "$cred_file")" != "install-credentials.txt" ]; then
                username=$(basename "$cred_file" .txt)
                writer_pass=$(extract_password "$cred_file" "WRITER USER")
                reader_pass=$(extract_password "$cred_file" "READER USER")
                database=$(grep "^Database:" "$cred_file" | head -1 | sed 's/Database: //' | tr -d '[:space:]')

                # URL encode passwords for connection strings
                encoded_writer=$(url_encode "${writer_pass}")
                encoded_reader=$(url_encode "${reader_pass}")
                
                echo ""
                echo -e "  ${YELLOW}[$username]${NC}"
                echo -e "    Database:      $database"
                echo -e "    Writer User:   ${username}_writer"
                echo -e "    Writer Pass:   $writer_pass"
                echo -e "    Reader User:   ${username}_reader"
                echo -e "    Reader Pass:   $reader_pass"
                echo -e "    Writer URI:    mongodb://${username}_writer:${encoded_writer}@${SERVER_IP}:27017/${database}?authSource=${database}&replicaSet=rs0"
                echo -e "    Reader URI:    mongodb://${username}_reader:${encoded_reader}@${SERVER_IP}:27017/${database}?authSource=${database}&replicaSet=rs0"
            fi
        done
        echo ""
    else
        echo -e "  ${YELLOW}Henüz kullanıcı oluşturulmamış.${NC}"
        echo -e "  Yeni kullanıcı oluşturmak için: $0 --create <isim>"
        echo ""
    fi

    # Also show main relay_agent info
    echo -e "${GREEN}▶ Ana Sistem Kullanıcısı (relay_agent):${NC}"
    echo "  ─────────────────────────────────────"
    echo -e "    User:       relay_agent"
    echo -e "    Password:   $RELAY_AGENT_PASS"
    echo -e "    Database:   relay_logs"
    echo -e "    Collection: emails"
    # URL encode relay_agent password
    encoded_relay=$(url_encode "${RELAY_AGENT_PASS}")
    echo -e "    URI:        mongodb://relay_agent:${encoded_relay}@${SERVER_IP}:27017/relay_logs?authSource=relay_logs&replicaSet=rs0"
    echo ""
}

# Delete user
delete_user() {
    local USERNAME="$1"

    if [ -z "$USERNAME" ]; then
        log_error "Kullanıcı adı belirtilmedi!"
        echo "Kullanım: $0 --delete <kullanıcı_adı>"
        exit 1
    fi

    USERNAME=$(echo "$USERNAME" | tr -cd '[:alnum:]_' | tr '[:upper:]' '[:lower:]')
    DATABASE="relay_${USERNAME}"

    load_credentials
    verify_admin_auth

    log_step "Kullanıcılar ve database siliniyor: $USERNAME"

    ADMIN_URI=$(get_admin_mongo_uri)

    # Delete both users and database using admin credentials
    mongosh "$ADMIN_URI" --quiet <<EOF
use $DATABASE
try { db.dropUser("${USERNAME}_writer"); print("Writer user dropped"); } catch(e) { print("Writer user not found"); }
try { db.dropUser("${USERNAME}_reader"); print("Reader user dropped"); } catch(e) { print("Reader user not found"); }
db.dropDatabase();
print("Database dropped");
EOF

    # Remove credentials file
    rm -f "/opt/relay-agent/credentials/${USERNAME}.txt"

    log_info "Kullanıcılar ve database silindi: $USERNAME"
}

# Show current configuration
show_config() {
    load_credentials

    SERVER_IP=$(get_server_ip)
    API_SECRET=$(grep "api_secret:" "$CONFIG_FILE" 2>/dev/null | head -1 | awk '{print $2}' | tr -d '"')

    echo ""
    echo -e "${CYAN}=========================================${NC}"
    echo -e "${CYAN}  YAPILANDIRMA BİLGİLERİ${NC}"
    echo -e "${CYAN}=========================================${NC}"
    echo ""

    echo -e "${GREEN}▶ Relay Agent API${NC}"
    echo "  ─────────────────────────────────────"
    echo -e "  URL:        ${YELLOW}http://$SERVER_IP:8080${NC}"
    echo -e "  API Secret: ${YELLOW}$API_SECRET${NC}"
    echo ""
    echo "  Örnek kullanım:"
    echo -e "  ${CYAN}curl -H \"X-API-Secret: $API_SECRET\" http://$SERVER_IP:8080/api/queue${NC}"
    echo ""

    echo -e "${GREEN}▶ MongoDB Ana Bağlantı (relay_agent)${NC}"
    echo "  ─────────────────────────────────────"
    echo -e "  Host:       ${YELLOW}$SERVER_IP${NC}"
    echo -e "  Port:       ${YELLOW}27017${NC}"
    echo -e "  Database:   ${YELLOW}relay_logs${NC}"
    echo -e "  Username:   ${YELLOW}relay_agent${NC}"
    echo -e "  Password:   ${YELLOW}$RELAY_AGENT_PASS${NC}"
    echo ""
    # URL encode password for display
    encoded_relay_pass=$(url_encode "${RELAY_AGENT_PASS}")
    echo "  Connection String:"
    echo -e "  ${CYAN}mongodb://relay_agent:${encoded_relay_pass}@$SERVER_IP:27017/relay_logs?authSource=relay_logs&replicaSet=rs0${NC}"
    echo ""

    echo -e "${GREEN}▶ Servis Durumu${NC}"
    echo "  ─────────────────────────────────────"
    printf "  MongoDB:     "
    if systemctl is-active --quiet mongod; then
        echo -e "${GREEN}Çalışıyor ✓${NC}"
    else
        echo -e "${RED}Durdu ✗${NC}"
    fi
    printf "  Relay-Agent: "
    if systemctl is-active --quiet relay-agent; then
        echo -e "${GREEN}Çalışıyor ✓${NC}"
    else
        echo -e "${RED}Durdu ✗${NC}"
    fi
    echo ""

    echo -e "${GREEN}▶ Dinlenen Portlar${NC}"
    echo "  ─────────────────────────────────────"
    ss -tlnp 2>/dev/null | grep -E "27017|8080|10025" | awk '{print "  " $4}' | sort -u
    echo ""

    # Show created users if any
    if [ -d /opt/relay-agent/credentials ]; then
        local has_users=false
        for cred_file in /opt/relay-agent/credentials/*.txt; do
            if [ -f "$cred_file" ] && [ "$(basename "$cred_file")" != "install-credentials.txt" ]; then
                has_users=true
                break
            fi
        done
        if [ "$has_users" = true ]; then
            echo -e "${GREEN}▶ Oluşturulan Ek Kullanıcılar${NC}"
            echo "  ─────────────────────────────────────"
            for cred_file in /opt/relay-agent/credentials/*.txt; do
                if [ -f "$cred_file" ] && [ "$(basename "$cred_file")" != "install-credentials.txt" ]; then
                    username=$(basename "$cred_file" .txt)
                    echo -e "  - ${YELLOW}$username${NC} (detay için: $0 --list)"
                fi
            done
            echo ""
        fi
    fi
}

# Main
check_root

case "${1:-}" in
    --create|-c)
        create_user "$2"
        ;;
    --list|-l)
        list_users
        ;;
    --delete|-d)
        delete_user "$2"
        ;;
    --help|-h)
        show_usage
        ;;
    "")
        show_config
        ;;
    *)
        log_error "Bilinmeyen parametre: $1"
        show_usage
        exit 1
        ;;
esac
