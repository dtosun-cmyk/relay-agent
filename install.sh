#!/bin/bash
set -e

# Colors
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m'

log_info() { echo -e "${GREEN}[INFO]${NC} $1"; }
log_warn() { echo -e "${YELLOW}[WARN]${NC} $1"; }
log_error() { echo -e "${RED}[ERROR]${NC} $1"; }

# Wait for MongoDB to accept connections (up to $1 seconds, default 30)
wait_for_mongod() {
    local max_wait="${1:-30}"
    local i
    for (( i=1; i<=max_wait; i++ )); do
        if mongosh --quiet --eval "db.runCommand({ping:1}).ok" 2>/dev/null | grep -q "1"; then
            return 0
        fi
        sleep 1
    done
    return 1
}

# Wait for replica set node to become writable primary (up to $1 seconds, default 60)
wait_for_primary() {
    local max_wait="${1:-60}"
    local i state
    for (( i=1; i<=max_wait; i++ )); do
        state=$(mongosh --quiet --eval 'JSON.stringify({primary: db.hello().isWritablePrimary, state: rs.status().myState})' 2>/dev/null || echo '{}')
        if echo "$state" | grep -q '"primary":true'; then
            log_info "MongoDB PRIMARY oldu (${i}s)"
            return 0
        fi
        if (( i % 10 == 0 )); then
            log_warn "MongoDB henuz PRIMARY degil (${i}/${max_wait}s) - durum: $state"
        fi
        sleep 1
    done
    log_error "MongoDB ${max_wait} saniye icinde PRIMARY olamadi!"
    log_error "Son durum: $state"
    return 1
}

# Safe password extraction from credentials file
extract_password() {
    local file="$1"
    local section="$2"
    local pass_line

    pass_line=$(grep -A5 "^${section}:" "$file" 2>/dev/null | grep "^Password:")
    if [ -n "$pass_line" ]; then
        echo "${pass_line#Password:}" | sed 's/^[[:space:]]*//'
    fi
}

# Detect primary server IPv4 (best-effort)
detect_primary_ip() {
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

# Build MongoDB bind IP list.
# Override with RELAY_MONGO_BIND_IPS="127.0.0.1,1.2.3.4"
build_mongo_bind_ips() {
    local bind_ips all_ipv4

    if [ -n "${RELAY_MONGO_BIND_IPS:-}" ]; then
        bind_ips=$(echo "${RELAY_MONGO_BIND_IPS}" | tr -d '[:space:]')
        if [[ ",${bind_ips}," != *",127.0.0.1,"* ]]; then
            bind_ips="127.0.0.1,${bind_ips}"
        fi
        echo "$bind_ips"
        return
    fi

    all_ipv4=$(hostname -I 2>/dev/null | tr ' ' '\n' | \
        grep -E '^[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+$' | \
        grep -v '^127\.' | awk '!seen[$0]++' | paste -sd, -)

    if [ -n "$all_ipv4" ]; then
        echo "127.0.0.1,${all_ipv4}"
    else
        echo "127.0.0.1"
    fi
}

# Default values
INSTALL_DIR="/opt/relay-agent"
MONGODB_VERSION="8.0"
API_PORT="8080"
SMTP_FILTER_PORT="10025"
REINJECTION_PORT="10026"

# GitHub Release settings
GITHUB_REPO="dtosun-cmyk/relay-agent"
BINARY_NAME="relay-agent-linux-amd64"

# Version: override with ./install.sh v1.0.1 or uses latest
RELEASE_TAG="${1:-latest}"

# Check root
if [ "$EUID" -ne 0 ]; then
    log_error "Root olarak calistirin: sudo ./install.sh"
    exit 1
fi

log_info "=== Relay Agent Kurulum Scripti ==="
echo ""

#######################################
# Interactive Domain Prompt
#######################################
validate_domain() {
    local domain="$1"
    # Must contain at least one dot
    if [[ ! "$domain" == *.* ]]; then
        return 1
    fi
    # No spaces
    if [[ "$domain" == *" "* ]]; then
        return 1
    fi
    # No leading or trailing dots
    if [[ "$domain" == .* ]] || [[ "$domain" == *. ]]; then
        return 1
    fi
    # Valid characters only (alphanumeric, hyphens, dots)
    if [[ ! "$domain" =~ ^[a-zA-Z0-9]([a-zA-Z0-9.-]*[a-zA-Z0-9])?$ ]]; then
        return 1
    fi
    return 0
}

# Detect hostname as default suggestion
DETECTED_HOSTNAME=$(hostname -f 2>/dev/null || hostname)
DEFAULT_PROMPT=""
if validate_domain "$DETECTED_HOSTNAME"; then
    DEFAULT_PROMPT=" [${DETECTED_HOSTNAME}]"
fi

# Support curl|bash: read from /dev/tty when stdin is a pipe
if [ -c /dev/tty ]; then
    READ_FROM="/dev/tty"
else
    READ_FROM="/dev/stdin"
fi

while true; do
    echo -e "${YELLOW}Enter the fully qualified domain name (FQDN) for this server${DEFAULT_PROMPT}:${NC}"
    echo -e "${YELLOW}  Example: mail.example.com${NC}"
    read -r DOMAIN_INPUT < "$READ_FROM" || DOMAIN_INPUT=""

    # Use default if empty and default is valid
    if [ -z "$DOMAIN_INPUT" ] && [ -n "$DEFAULT_PROMPT" ]; then
        DOMAIN="${DETECTED_HOSTNAME}"
    else
        DOMAIN="${DOMAIN_INPUT}"
    fi

    if validate_domain "$DOMAIN"; then
        log_info "Domain: ${DOMAIN}"
        echo ""
        break
    else
        log_error "Invalid domain: '${DOMAIN}'. Must be a valid FQDN (e.g. mail.example.com)"
        echo ""
    fi
done

# Detect OS
if [ -f /etc/os-release ]; then
    . /etc/os-release
    OS=$ID
    VERSION_ID=$VERSION_ID
else
    log_error "Desteklenmeyen isletim sistemi"
    exit 1
fi

log_info "Isletim Sistemi: $OS $VERSION_ID"

# Detect Server IP / bind list for MongoDB
SERVER_IP=$(detect_primary_ip)
MONGO_BIND_IPS=$(build_mongo_bind_ips)
log_info "Sunucu IP (primary): $SERVER_IP"
log_info "MongoDB bindIp listesi: $MONGO_BIND_IPS"

#######################################
# 1. System Dependencies
#######################################
log_info "Sistem bagimliliklari kuruluyor..."

apt-get update -qq
apt-get install -y -qq curl wget gnupg lsb-release openssl certbot rsyslog

#######################################
# 2. MongoDB Installation
#######################################
if command -v mongod &> /dev/null; then
    log_info "MongoDB zaten kurulu"
else
    log_info "MongoDB $MONGODB_VERSION kuruluyor..."

    # Add MongoDB repo
    curl -fsSL https://www.mongodb.org/static/pgp/server-${MONGODB_VERSION}.asc | \
        gpg -o /usr/share/keyrings/mongodb-server-${MONGODB_VERSION}.gpg --dearmor

    echo "deb [ arch=amd64,arm64 signed-by=/usr/share/keyrings/mongodb-server-${MONGODB_VERSION}.gpg ] https://repo.mongodb.org/apt/ubuntu $(lsb_release -cs)/mongodb-org/${MONGODB_VERSION} multiverse" | \
        tee /etc/apt/sources.list.d/mongodb-org-${MONGODB_VERSION}.list

    apt-get update -qq
    apt-get install -y -qq mongodb-org

    log_info "MongoDB kuruldu"
fi

#######################################
# 3. MongoDB Replica Set + Auth
#######################################
log_info "MongoDB Replica Set + Auth yapilandiriliyor..."

# Generate random passwords
CRED_FILE="${INSTALL_DIR}/credentials/install-credentials.txt"
if [ -f "$CRED_FILE" ]; then
    EXISTING_ADMIN_PASS=$(extract_password "$CRED_FILE" "MONGODB ADMIN")
    EXISTING_RELAY_PASS=$(extract_password "$CRED_FILE" "MONGODB RELAY_AGENT")
fi

if [ -n "${EXISTING_ADMIN_PASS:-}" ] && [ -n "${EXISTING_RELAY_PASS:-}" ]; then
    MONGO_ADMIN_PASS="${EXISTING_ADMIN_PASS}"
    MONGO_RELAY_PASS="${EXISTING_RELAY_PASS}"
    log_info "Mevcut MongoDB credential'lari bulundu, aynilari kullanilacak"
else
    MONGO_ADMIN_PASS=$(openssl rand -base64 48 | tr -d '/+=' | head -c 32)
    MONGO_RELAY_PASS=$(openssl rand -base64 48 | tr -d '/+=' | head -c 32)
fi

# Create keyfile for replica set auth
# Ensure MongoDB data directory exists
if [ ! -d /var/lib/mongodb ]; then
    mkdir -p /var/lib/mongodb
    chown mongodb:mongodb /var/lib/mongodb
fi

if [ ! -f /var/lib/mongodb/keyfile ]; then
    openssl rand -base64 756 > /var/lib/mongodb/keyfile
    chmod 400 /var/lib/mongodb/keyfile
    chown mongodb:mongodb /var/lib/mongodb/keyfile
fi

# Configure mongod.conf WITHOUT auth first (for initial setup)
# Bind to localhost + detected server IPv4 addresses
cat > /etc/mongod.conf << EOF
storage:
  dbPath: /var/lib/mongodb

systemLog:
  destination: file
  logAppend: true
  path: /var/log/mongodb/mongod.log

net:
  port: 27017
  bindIp: ${MONGO_BIND_IPS}

processManagement:
  timeZoneInfo: /usr/share/zoneinfo

replication:
  replSetName: rs0
EOF

# Start MongoDB without auth
systemctl enable mongod
systemctl restart mongod

# Wait for mongod to accept connections before any operations
log_info "MongoDB baslatilmasi bekleniyor..."
if ! wait_for_mongod 30; then
    log_error "MongoDB baslatilamadi veya baglanti kurulamadi!"
    log_error "Kontrol edin: journalctl -u mongod -n 50"
    exit 1
fi
log_info "MongoDB baglantisi basarili"

# Initialize (or fix) single-node replica set host
log_info "Replica set yapilandiriliyor (host: ${SERVER_IP}:27017)..."
mongosh --quiet << EOF
const wantedHost = "${SERVER_IP}:27017";

function initOrReconfig() {
    try {
        const status = rs.status();
        if (status.ok === 1) {
            const cfg = rs.conf();
            if (cfg.members && cfg.members.length === 1 && cfg.members[0].host !== wantedHost) {
                cfg.members[0].host = wantedHost;
                cfg.version = cfg.version + 1;
                rs.reconfig(cfg, { force: true });
                print("rs0 reconfigured to " + wantedHost);
            } else {
                print("rs0 already configured correctly");
            }
            return true;
        }
    } catch(e) {
        // rs.status() fails when no replica set exists yet
    }

    try {
        rs.initiate({ _id: "rs0", members: [{ _id: 0, host: wantedHost }] });
        print("rs0 initiated with " + wantedHost);
        return true;
    } catch(e) {
        print("rs.initiate failed: " + e.message);
        return false;
    }
}

if (!initOrReconfig()) {
    sleep(2000);
    if (!initOrReconfig()) {
        print("ERROR: Replica set setup failed after retry");
        quit(1);
    }
}
EOF

# Wait for node to become PRIMARY — this is critical before user creation
if ! wait_for_primary 60; then
    log_error "Replica set election tamamlanamadi."
    log_error "Muhtemel neden: SERVER_IP ($SERVER_IP) MongoDB'nin dinledigi adreslerle uyusmuyor."
    log_error "bindIp: $MONGO_BIND_IPS"
    log_error "Kontrol: mongosh --eval 'rs.status()'"
    exit 1
fi

# Create admin user in admin db
log_info "MongoDB admin kullanicisi olusturuluyor..."
mongosh --quiet << EOF
use admin
try {
    if (db.getUser("admin")) {
        db.updateUser("admin", { pwd: "${MONGO_ADMIN_PASS}", roles: ["root"] });
        print("admin user updated");
    } else {
        db.createUser({
            user: "admin",
            pwd: "${MONGO_ADMIN_PASS}",
            roles: ["root"]
        });
        print("admin user created");
    }
} catch(e) {
    print("admin user setup failed: " + e.message);
    quit(1);
}
EOF

# Create relay_agent user in relay_logs db
log_info "MongoDB relay_agent kullanicisi olusturuluyor..."
mongosh --quiet << EOF
use relay_logs
db.createCollection("emails")
try {
    if (db.getUser("relay_agent")) {
        db.updateUser("relay_agent", {
            pwd: "${MONGO_RELAY_PASS}",
            roles: [{ role: "readWrite", db: "relay_logs" }]
        });
        print("relay_agent user updated");
    } else {
        db.createUser({
            user: "relay_agent",
            pwd: "${MONGO_RELAY_PASS}",
            roles: [{ role: "readWrite", db: "relay_logs" }]
        });
        print("relay_agent user created");
    }
} catch(e) {
    print("relay_agent user setup failed: " + e.message);
    quit(1);
}
EOF

# Rewrite mongod.conf WITH auth enabled
log_info "MongoDB auth etkinlestiriliyor..."
cat > /etc/mongod.conf << EOF
storage:
  dbPath: /var/lib/mongodb

systemLog:
  destination: file
  logAppend: true
  path: /var/log/mongodb/mongod.log

net:
  port: 27017
  bindIp: ${MONGO_BIND_IPS}

processManagement:
  timeZoneInfo: /usr/share/zoneinfo

security:
  authorization: enabled
  keyFile: /var/lib/mongodb/keyfile

replication:
  replSetName: rs0
EOF

# Restart MongoDB with auth
systemctl restart mongod

# Wait for mongod to accept connections after auth restart
log_info "MongoDB (auth modunda) baslatilmasi bekleniyor..."
if ! wait_for_mongod 30; then
    log_error "MongoDB auth modunda baslatilamadi!"
    log_error "Kontrol edin: journalctl -u mongod -n 50"
    exit 1
fi

# Wait for primary with auth — use admin credentials
log_info "Replica set PRIMARY bekleniyor (auth modunda)..."
RS_READY=false
for i in $(seq 1 60); do
    if mongosh "mongodb://admin:${MONGO_ADMIN_PASS}@localhost:27017/admin?authSource=admin&replicaSet=rs0&directConnection=true" \
        --quiet --eval 'db.hello().isWritablePrimary' 2>/dev/null | grep -q "true"; then
        RS_READY=true
        log_info "MongoDB PRIMARY oldu (auth modunda, ${i}s)"
        break
    fi
    if (( i % 10 == 0 )); then
        log_warn "MongoDB henuz PRIMARY degil (auth modunda, ${i}/60s)"
    fi
    sleep 1
done

if [ "$RS_READY" != "true" ]; then
    log_error "MongoDB auth modunda PRIMARY olamadi!"
    exit 1
fi

# Verify relay_agent auth works
if mongosh "mongodb://relay_agent:${MONGO_RELAY_PASS}@localhost:27017/relay_logs?authSource=relay_logs&replicaSet=rs0&directConnection=true" --quiet --eval "db.runCommand({ping:1}).ok" 2>/dev/null | grep -q "1"; then
    log_info "MongoDB auth dogrulandi (relay_agent)"
else
    log_error "MongoDB relay_agent auth dogrulanamadi. Kurulum durduruluyor."
    exit 1
fi

log_info "MongoDB Replica Set + Auth yapilandirildi"

#######################################
# 4. Let's Encrypt SSL
#######################################
log_info "SSL sertifikasi aliniyor (Let's Encrypt)..."

TLS_OBTAINED="no"

# Stop services that might hold port 80
systemctl stop apache2 2>/dev/null || true
systemctl stop nginx 2>/dev/null || true

# Check if user wants to provide email for Let's Encrypt
LE_EMAIL=""
if [ -n "${RELAY_LETSENCRYPT_EMAIL:-}" ]; then
    LE_EMAIL="${RELAY_LETSENCRYPT_EMAIL}"
    log_info "Let's Encrypt email: ${LE_EMAIL}"
elif [ -c /dev/tty ]; then
    # Interactive mode - ask user (works with both ./install.sh and curl|bash)
    echo ""
    echo -e "${YELLOW}Let's Encrypt email adresi (opsiyonel, bos birakabilirsiniz):${NC}"
    read -r LE_EMAIL_INPUT < "$READ_FROM" || LE_EMAIL_INPUT=""
    if [ -n "${LE_EMAIL_INPUT}" ]; then
        LE_EMAIL="${LE_EMAIL_INPUT}"
    fi
fi

if [ -n "${LE_EMAIL}" ]; then
    log_info "Let's Encrypt sertifikasi email ile aliniyor: ${LE_EMAIL}..."
    if certbot certonly --standalone -d "${DOMAIN}" --non-interactive --agree-tos -m "${LE_EMAIL}" 2>/dev/null; then
        TLS_CERT_FILE="/etc/letsencrypt/live/${DOMAIN}/fullchain.pem"
        TLS_KEY_FILE="/etc/letsencrypt/live/${DOMAIN}/privkey.pem"
        TLS_OBTAINED="yes"
        log_info "SSL sertifikasi alindi: ${TLS_CERT_FILE}"
    else
        log_warn "Let's Encrypt email ile sertifika alinamadi. Email olmadan deneniyor..."
        if certbot certonly --standalone -d "${DOMAIN}" --non-interactive --agree-tos --register-unsafely-without-email 2>/dev/null; then
            TLS_CERT_FILE="/etc/letsencrypt/live/${DOMAIN}/fullchain.pem"
            TLS_KEY_FILE="/etc/letsencrypt/live/${DOMAIN}/privkey.pem"
            TLS_OBTAINED="yes"
            log_info "SSL sertifikasi alindi: ${TLS_CERT_FILE}"
        else
            log_warn "Let's Encrypt sertifikasi alinamadi. Snakeoil sertifikasi kullanilacak."
            log_warn "DNS kaydi kontrol edin: ${DOMAIN} -> sunucu IP"
            TLS_CERT_FILE="/etc/ssl/certs/ssl-cert-snakeoil.pem"
            TLS_KEY_FILE="/etc/ssl/private/ssl-cert-snakeoil.key"
            TLS_OBTAINED="no"
        fi
    fi
elif certbot certonly --standalone -d "${DOMAIN}" --non-interactive --agree-tos --register-unsafely-without-email 2>/dev/null; then
    TLS_CERT_FILE="/etc/letsencrypt/live/${DOMAIN}/fullchain.pem"
    TLS_KEY_FILE="/etc/letsencrypt/live/${DOMAIN}/privkey.pem"
    TLS_OBTAINED="yes"
    log_info "SSL sertifikasi alindi: ${TLS_CERT_FILE}"
else
    log_warn "Let's Encrypt sertifikasi alinamadi. Snakeoil sertifikasi kullanilacak."
    log_warn "DNS kaydi kontrol edin: ${DOMAIN} -> sunucu IP"
    log_warn "Tekrar denemek icin: certbot certonly --standalone -d ${DOMAIN}"
    TLS_CERT_FILE="/etc/ssl/certs/ssl-cert-snakeoil.pem"
    TLS_KEY_FILE="/etc/ssl/private/ssl-cert-snakeoil.key"
    TLS_OBTAINED="no"
fi

#######################################
# 5. Postfix Installation & Configuration
#######################################
if command -v postfix &> /dev/null; then
    log_info "Postfix zaten kurulu"
else
    log_info "Postfix kuruluyor..."

    # Non-interactive Postfix installation
    debconf-set-selections <<< "postfix postfix/mailname string ${DOMAIN}"
    debconf-set-selections <<< "postfix postfix/main_mailer_type string 'Internet Site'"

    apt-get install -y -qq postfix sasl2-bin libsasl2-modules

    log_info "Postfix kuruldu"
fi

log_info "Postfix yapilandiriliyor..."

SMTP_HOSTNAME="${DOMAIN}"

# Ensure mail.log exists for parser
if [ ! -f /var/log/mail.log ]; then
    touch /var/log/mail.log
fi
chown syslog:adm /var/log/mail.log
chmod 640 /var/log/mail.log

#--- rsyslog: route Postfix logs to /var/log/mail.log ---
log_info "rsyslog mail facility yapilandiriliyor..."

cat > /etc/rsyslog.d/20-postfix.conf << 'RSYSEOF'
# Route all mail facility logs to /var/log/mail.log for relay-agent parser
# Use traditional BSD syslog format (Feb 24 21:26:46) required by relay-agent parser
mail.*    -/var/log/mail.log;RSYSLOG_TraditionalFileFormat

# Prevent mail logs from also going to /var/log/syslog (optional, reduces noise)
& stop
RSYSEOF

systemctl restart rsyslog 2>/dev/null || true
log_info "rsyslog yapilandirildi: mail.* -> /var/log/mail.log"

#--- logrotate: rotate mail.log ---
cat > /etc/logrotate.d/mail-log << 'ROTATEEOF'
/var/log/mail.log {
    daily
    missingok
    rotate 14
    compress
    delaycompress
    notifempty
    create 0640 syslog adm
    sharedscripts
    postrotate
        # Signal rsyslog to reopen log files
        /usr/lib/rsyslog/rsyslog-rotate 2>/dev/null || invoke-rc.d rsyslog rotate 2>/dev/null || true
    endscript
}
ROTATEEOF

log_info "logrotate yapilandirildi: /var/log/mail.log (gunluk, 14 gun)"

# Ensure postfix spool directory exists
mkdir -p /var/spool/postfix

#--- SASL Authentication Setup ---
log_info "SASL authentication yapilandiriliyor..."

# Create SASL config directory and file
mkdir -p /etc/postfix/sasl
cat > /etc/postfix/sasl/smtpd.conf << 'EOF'
pwcheck_method: auxprop
auxprop_plugin: sasldb
mech_list: PLAIN LOGIN
sasldb_path: /etc/sasldb2
EOF

# Ensure sasldb2 exists with correct permissions
if [ ! -f /etc/sasldb2 ]; then
    touch /etc/sasldb2
fi
chown root:postfix /etc/sasldb2
chmod 640 /etc/sasldb2

log_info "SASL config olusturuldu: /etc/postfix/sasl/smtpd.conf"

#--- main.cf Configuration ---
log_info "Postfix main.cf yapilandiriliyor..."

# SASL authentication
postconf -e "smtpd_sasl_auth_enable = yes"
postconf -e "smtpd_sasl_type = cyrus"
postconf -e "smtpd_sasl_path = smtpd"
postconf -e "smtpd_sasl_security_options = noanonymous"
postconf -e "smtpd_sasl_local_domain = \$myhostname"
postconf -e "broken_sasl_auth_clients = yes"

# Relay restrictions (allow authenticated users to relay)
postconf -e "smtpd_relay_restrictions = permit_mynetworks permit_sasl_authenticated defer_unauth_destination"

# Content filter -> relay-agent SMTP filter
postconf -e "content_filter = smtp:[127.0.0.1]:${SMTP_FILTER_PORT}"

# Disable SMTPUTF8 for compatibility (Yandex, etc.)
postconf -e "smtputf8_enable = no"

# Set hostname and destination
postconf -e "myhostname = ${SMTP_HOSTNAME}"
postconf -e "mydestination = ${DOMAIN}, localhost.localdomain, localhost"

# TLS certificate and key
postconf -e "smtpd_tls_cert_file = ${TLS_CERT_FILE}"
postconf -e "smtpd_tls_key_file = ${TLS_KEY_FILE}"
postconf -e "smtpd_tls_security_level = may"

# TLS protocol restrictions (disable insecure protocols)
postconf -e "smtpd_tls_mandatory_protocols = !SSLv2, !SSLv3, !TLSv1, !TLSv1.1"
postconf -e "smtpd_tls_protocols = !SSLv2, !SSLv3, !TLSv1, !TLSv1.1"

# Outbound delivery - standard port 25 to destination MX
postconf -e "relayhost ="
postconf -e "smtp_tls_security_level = may"

# XFORWARD: pass original client info (IP, hostname) to content_filter (relay-agent)
postconf -e "smtp_send_xforward_command = yes"
postconf -e "smtpd_authorized_xforward_hosts = 127.0.0.0/8"

#--- master.cf Configuration ---
log_info "Postfix master.cf yapilandiriliyor..."

# Disable port 25 - all traffic goes through 587
if grep -q "^smtp      inet" /etc/postfix/master.cf; then
    sed -i 's/^smtp      inet/#smtp      inet/' /etc/postfix/master.cf
    log_info "Port 25 (smtp) devre disi birakildi"
fi

# Add submission port (587) with SASL auth, chroot=n for sasldb2 access
# Check both submission and our specific configuration
if ! grep -q "^submission.*smtpd$" /etc/postfix/master.cf; then
    cat >> /etc/postfix/master.cf << 'EOF'

# Submission port (587) - SASL authenticated relay, chroot=n for sasldb2
submission inet  n  -  n  -  -  smtpd
    -o syslog_name=postfix/submission
    -o smtpd_tls_security_level=encrypt
    -o smtpd_sasl_auth_enable=yes
    -o smtpd_tls_auth_only=yes
    -o smtpd_reject_unlisted_recipient=no
    -o smtpd_relay_restrictions=permit_sasl_authenticated,reject
    -o milter_macro_daemon_name=ORIGINATING
EOF
    log_info "Submission port (587) eklendi"
else
    # Check if our specific config exists
    if ! grep -q "smtpd_relay_restrictions=permit_sasl_authenticated,reject" /etc/postfix/master.cf; then
        log_warn "Submission port var ama SASL ayarlari eksik. Yeniden ekleniyor..."
        # Remove existing submission line and add our config
        sed -i '/^submission inet/,/^EOF$/d' /etc/postfix/master.cf 2>/dev/null || true
        cat >> /etc/postfix/master.cf << 'EOF'

# Submission port (587) - SASL authenticated relay, chroot=n for sasldb2
submission inet  n  -  n  -  -  smtpd
    -o syslog_name=postfix/submission
    -o smtpd_tls_security_level=encrypt
    -o smtpd_sasl_auth_enable=yes
    -o smtpd_tls_auth_only=yes
    -o smtpd_reject_unlisted_recipient=no
    -o smtpd_relay_restrictions=permit_sasl_authenticated,reject
    -o milter_macro_daemon_name=ORIGINATING
EOF
        log_info "Submission port (587) guncellendi"
    fi
fi

# Add reinjection service for relay-agent
# Port ${SMTP_FILTER_PORT} is owned by relay-agent (SMTP filter), NOT Postfix
# Port ${REINJECTION_PORT} is owned by Postfix (reinjection after filter)
if ! grep -q "reinjection.*${REINJECTION_PORT}" /etc/postfix/master.cf 2>/dev/null; then
    cat >> /etc/postfix/master.cf << EOF

# Reinjection service - accepts filtered mail from relay-agent and delivers
# relay-agent listens on ${SMTP_FILTER_PORT} (not managed by Postfix)
127.0.0.1:${REINJECTION_PORT} inet n - n - 30 smtpd
    -o content_filter=
    -o receive_override_options=no_unknown_recipient_checks,no_header_body_checks
    -o smtpd_helo_restrictions=
    -o smtpd_client_restrictions=
    -o smtpd_sender_restrictions=
    -o smtpd_recipient_restrictions=permit_mynetworks,reject
    -o mynetworks=127.0.0.0/8
    -o smtpd_authorized_xforward_hosts=127.0.0.0/8
    -o syslog_name=postfix/reinjection
EOF
    log_info "Reinjection servisi (${REINJECTION_PORT}) eklendi"
else
    log_info "Reinjection servisi zaten mevcut, atlaniyor"
fi

# Remove stale relay-agent-filter entry from master.cf if present
# Port ${SMTP_FILTER_PORT} must NOT be a Postfix service - relay-agent owns it
if grep -q "^127.0.0.1:${SMTP_FILTER_PORT}.*inet" /etc/postfix/master.cf 2>/dev/null; then
    log_info "Port ${SMTP_FILTER_PORT} Postfix master.cf'den kaldiriliyor (relay-agent'a ait)..."
    # Remove the filter block (from port line to next blank line or service)
    sed -i "/^127\.0\.0\.1:${SMTP_FILTER_PORT}.*inet/,/^$/d" /etc/postfix/master.cf
    # Clean up any orphaned relay-agent-filter comment
    sed -i '/^# Relay Agent SMTP Filter$/d' /etc/postfix/master.cf
    log_info "Port ${SMTP_FILTER_PORT} Postfix'ten kaldirildi"
fi

# Enable and start Postfix
systemctl enable postfix
systemctl restart postfix

log_info "Postfix yapilandirildi"
log_info "  - Giris: 587 (Submission, SASL + TLS)"
log_info "  - Cikis: 25 (standart MX teslim)"
log_info "  - Port 25 dinleme: KAPALI (sadece cikis icin)"
log_info "  - SMTP Filter: ${SMTP_FILTER_PORT} (relay-agent)"
log_info "  - Reinjection: ${REINJECTION_PORT} (Postfix)"
log_info "  - Content Filter: smtp:[127.0.0.1]:${SMTP_FILTER_PORT}"

#######################################
# 6. Download Relay Agent Binary
#######################################
log_info "Relay Agent binary indiriliyor..."

mkdir -p "$INSTALL_DIR/bin" "$INSTALL_DIR/logs" "$INSTALL_DIR/config" "$INSTALL_DIR/credentials"
mkdir -p /var/lib/relay-agent

# Resolve download URL
if [ "$RELEASE_TAG" = "latest" ]; then
    DOWNLOAD_URL="https://github.com/${GITHUB_REPO}/releases/latest/download/${BINARY_NAME}"
else
    DOWNLOAD_URL="https://github.com/${GITHUB_REPO}/releases/download/${RELEASE_TAG}/${BINARY_NAME}"
fi

log_info "Indiriliyor: $DOWNLOAD_URL"

# Download binary
if ! curl -fSL -o "$INSTALL_DIR/bin/relay-agent" "$DOWNLOAD_URL"; then
    log_error "Binary indirilemedi! URL: $DOWNLOAD_URL"
    log_error "Releases sayfasini kontrol edin: https://github.com/${GITHUB_REPO}/releases"
    log_info ""
    log_info "Manuel build yapabilirsiniz:"
    log_info "  1. cd /opt/relay-agent"
    log_info "  2. go build -o bin/relay-agent ./cmd/relay-agent"
    log_info "  3. systemctl restart relay-agent"
    exit 1
fi

chmod +x "$INSTALL_DIR/bin/relay-agent"
log_info "Relay Agent binary indirildi: $INSTALL_DIR/bin/relay-agent"

# Download setup-mailgateway-access.sh
log_info "Mailgateway setup scripti indiriliyor..."
if [ "$RELEASE_TAG" = "latest" ]; then
    SCRIPT_BRANCH="master"
else
    SCRIPT_BRANCH="$RELEASE_TAG"
fi

if curl -fsSL -o "$INSTALL_DIR/setup-mailgateway-access.sh" \
    "https://raw.githubusercontent.com/${GITHUB_REPO}/${SCRIPT_BRANCH}/setup-mailgateway-access.sh"; then
    chmod +x "$INSTALL_DIR/setup-mailgateway-access.sh"
    log_info "Mailgateway setup scripti indirildi: $INSTALL_DIR/setup-mailgateway-access.sh"
else
    log_warn "Mailgateway setup scripti indirilemedi, atlaniyor..."
fi

#######################################
# 7. Create Default Config
#######################################
if [ ! -f "$INSTALL_DIR/config/config.yaml" ]; then
    log_info "Varsayilan yapilandirma olusturuluyor..."

    # Generate random API secret
    API_SECRET=$(openssl rand -base64 24 | tr -d '/+=' | head -c 32)

    cat > "$INSTALL_DIR/config/config.yaml" << EOF
# Relay Agent Configuration
# Generated: $(date)

server:
  host: "0.0.0.0"
  port: ${API_PORT}

mongodb:
  uri: "mongodb://relay_agent:${MONGO_RELAY_PASS}@localhost:27017/relay_logs?authSource=relay_logs&replicaSet=rs0"
  database: "relay_logs"

mailgateway:
  relay_server_id: 1

postfix:
  log_file: "/var/log/mail.log"

smtp:
  domain: "${DOMAIN}"
  api_secret: "${API_SECRET}"

filter:
  enabled: true
  listen_addr: "127.0.0.1:${SMTP_FILTER_PORT}"
  next_hop: "127.0.0.1:${REINJECTION_PORT}"
  hostname: "${DOMAIN}"

processing:
  batch_size: 100
  flush_interval: 5
  channel_buffer: 1000

logging:
  level: "info"
  file: "${INSTALL_DIR}/logs/agent.log"
EOF

    log_info "Config olusturuldu: $INSTALL_DIR/config/config.yaml"
fi

#######################################
# 8. Create Systemd Service
#######################################
log_info "Systemd servisi olusturuluyor..."

cat > /etc/systemd/system/relay-agent.service << EOF
[Unit]
Description=Relay Agent - Postfix Log Parser and Management Service
Documentation=https://github.com/${GITHUB_REPO}
After=network.target mongod.service
Wants=mongod.service

[Service]
Type=simple
User=root
Group=root
WorkingDirectory=${INSTALL_DIR}
ExecStart=${INSTALL_DIR}/bin/relay-agent -config ${INSTALL_DIR}/config/config.yaml
ExecReload=/bin/kill -HUP \$MAINPID
Restart=always
RestartSec=5
StandardOutput=append:${INSTALL_DIR}/logs/agent.log
StandardError=append:${INSTALL_DIR}/logs/agent.log

# Security
NoNewPrivileges=true
ProtectSystem=strict
ReadWritePaths=${INSTALL_DIR}/logs /var/lib/relay-agent /etc/sasldb2 /var/log /var/spool/postfix /tmp

# Resource limits
LimitNOFILE=65536
LimitNPROC=4096

# Environment
Environment=GOMAXPROCS=4

[Install]
WantedBy=multi-user.target
EOF

systemctl daemon-reload
systemctl enable relay-agent

log_info "Systemd servisi olusturuldu"

#######################################
# 9. Start Service
#######################################
log_info "Relay Agent baslatiliyor..."

systemctl start relay-agent
sleep 2

if systemctl is-active --quiet relay-agent; then
    log_info "Relay Agent calisiyor"
else
    log_error "Relay Agent baslatilamadi. Loglar: journalctl -u relay-agent -f"
fi

#######################################
# 10. Save Credentials File
#######################################
log_info "Credentials dosyasi kaydediliyor..."

CRED_DIR="${INSTALL_DIR}/credentials"
CRED_FILE="${CRED_DIR}/install-credentials.txt"
mkdir -p "$CRED_DIR"
chmod 700 "$CRED_DIR"

# Read API_SECRET from config if not set (e.g. config already existed)
if [ -z "${API_SECRET:-}" ]; then
    API_SECRET=$(grep "api_secret:" "$INSTALL_DIR/config/config.yaml" 2>/dev/null | head -1 | awk '{print $2}' | tr -d '"')
fi

cat > "$CRED_FILE" << EOF
# Relay Agent Install Credentials
# Generated: $(date)
# Domain: ${DOMAIN}

DOMAIN INFORMATION:
===================
Domain:       ${DOMAIN}
Server IP:    ${SERVER_IP}

API INFORMATION:
================
API Address:  http://${SERVER_IP}:${API_PORT}
API Secret:   ${API_SECRET}

MONGODB ADMIN:
==============
Username:     admin
Password:     ${MONGO_ADMIN_PASS}
Auth DB:      admin

Connection String:
mongodb://admin:${MONGO_ADMIN_PASS}@${SERVER_IP}:27017/admin?authSource=admin&replicaSet=rs0

MONGODB RELAY_AGENT:
====================
Username:     relay_agent
Password:     ${MONGO_RELAY_PASS}
Database:     relay_logs
Auth DB:      relay_logs

Connection String:
mongodb://relay_agent:${MONGO_RELAY_PASS}@${SERVER_IP}:27017/relay_logs?authSource=relay_logs&replicaSet=rs0

SSL CERTIFICATE:
================
Status:       $([ "$TLS_OBTAINED" = "yes" ] && echo "Let's Encrypt" || echo "Snakeoil (self-signed)")
Cert File:    ${TLS_CERT_FILE}
Key File:     ${TLS_KEY_FILE}
EOF

chmod 600 "$CRED_FILE"
log_info "Credentials kaydedildi: ${CRED_FILE}"

#######################################
# Summary
#######################################
echo ""
echo "=========================================="
echo -e "${GREEN}Kurulum Tamamlandi!${NC}"
echo "=========================================="
echo ""
echo "Domain:       ${DOMAIN}"
echo "Server IP:    ${SERVER_IP}"
echo ""
echo "Yapilandirma: ${INSTALL_DIR}/config/config.yaml"
echo "Credentials:  ${CRED_FILE}"
echo "Loglar:       ${INSTALL_DIR}/logs/agent.log"
echo "API Port:     ${API_PORT}"
echo "SMTP Filter:  ${SMTP_FILTER_PORT}"
echo ""
echo "API:"
echo "  Address:    http://${SERVER_IP}:${API_PORT}"
echo "  Secret:     ${API_SECRET}"
echo "  Test:       curl -H 'X-API-Secret: ${API_SECRET}' http://localhost:${API_PORT}/api/queue"
echo ""
echo "MongoDB:"
echo "  Admin Pass:       ${MONGO_ADMIN_PASS}"
echo "  Relay Agent Pass: ${MONGO_RELAY_PASS}"
echo "  Auth:             enabled"
echo ""
if [ "$TLS_OBTAINED" = "yes" ]; then
    echo -e "SSL: ${GREEN}Let's Encrypt sertifikasi aktif${NC}"
    echo "  Cert: ${TLS_CERT_FILE}"
    echo "  Key:  ${TLS_KEY_FILE}"
else
    echo -e "SSL: ${YELLOW}Snakeoil sertifikasi (self-signed)${NC}"
    echo ""
    echo "  Let's Encrypt icin manuel kurulum:"
    echo "    certbot certonly --standalone -d ${DOMAIN}"
    echo "    postconf -e 'smtpd_tls_cert_file=/etc/letsencrypt/live/${DOMAIN}/fullchain.pem'"
    echo "    postconf -e 'smtpd_tls_key_file=/etc/letsencrypt/live/${DOMAIN}/privkey.pem'"
    echo "    systemctl restart postfix"
fi
echo ""
echo "SASL Kullanici Olusturma:"
echo "  echo 'SIFRE' | saslpasswd2 -c -p -u ${DOMAIN} USERNAME"
echo "  sasldblistusers2  # Kullanicilari listele"
echo ""
echo "Servis Komutlari:"
echo "  systemctl status relay-agent"
echo "  systemctl restart relay-agent"
echo "  journalctl -u relay-agent -f"
echo ""
echo "MongoDB Kullanici Yonetimi:"
echo "  ${INSTALL_DIR}/setup-mailgateway-access.sh --create <isim> # Yeni musteri olustur"
echo "  ${INSTALL_DIR}/setup-mailgateway-access.sh --list          # Kullanicilari listele"
echo "  ${INSTALL_DIR}/setup-mailgateway-access.sh --delete <isim> # Kullanici sil"
echo ""
