#!/bin/bash
set -e

RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m'

log_info() { echo -e "${GREEN}[INFO]${NC} $1"; }
log_warn() { echo -e "${YELLOW}[WARN]${NC} $1"; }
log_error() { echo -e "${RED}[ERROR]${NC} $1"; }

INSTALL_DIR="/opt/relay-agent"
CRED_FILE="${INSTALL_DIR}/credentials/install-credentials.txt"

# Check root
if [ "$EUID" -ne 0 ]; then
    log_error "Root olarak calistirin: sudo ./update-ip.sh"
    exit 1
fi

# Detect new IP
NEW_IP=$(ip -4 route get 1.1.1.1 2>/dev/null | awk '/src/ {for(i=1;i<=NF;i++) if($i=="src"){print $(i+1); exit}}')
if [ -z "$NEW_IP" ]; then
    NEW_IP=$(hostname -I 2>/dev/null | tr ' ' '\n' | grep -E '^[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+$' | grep -v '^127\.' | head -1)
fi

if [ -z "$NEW_IP" ]; then
    log_error "Sunucu IP adresi tespit edilemedi!"
    exit 1
fi

# Get old IP from MongoDB config (more reliable than credentials file)
OLD_IP=""
OLD_IP=$(grep "bindIp:" /etc/mongod.conf 2>/dev/null | sed 's/.*bindIp:[[:space:]]*//' | tr ',' '\n' | grep -v '127.0.0.1' | head -1)
if [ -z "$OLD_IP" ] && [ -f "$CRED_FILE" ]; then
    OLD_IP=$(grep "Server IP:" "$CRED_FILE" | head -1 | awk '{print $NF}')
fi

echo ""
echo "=========================================="
echo -e "${YELLOW}Relay Agent - IP Guncelleme${NC}"
echo "=========================================="
echo ""
echo "  Tespit edilen IP: ${NEW_IP}"
if [ -n "$OLD_IP" ]; then
    echo "  Mevcut (eski) IP: ${OLD_IP}"
fi
echo ""

if [ "$NEW_IP" = "$OLD_IP" ]; then
    log_info "IP degismemis, guncelleme gerekmiyor."
    exit 0
fi

# Extract MongoDB admin password
MONGO_ADMIN_PASS=""
if [ -f "$CRED_FILE" ]; then
    MONGO_ADMIN_PASS=$(grep -A5 "^MONGODB ADMIN:" "$CRED_FILE" | grep "^Password:" | sed 's/^Password:[[:space:]]*//')
fi

if [ -z "$MONGO_ADMIN_PASS" ]; then
    log_error "MongoDB admin sifresi bulunamadi: $CRED_FILE"
    log_error "Manuel olarak girin:"
    read -r -p "MongoDB admin password: " MONGO_ADMIN_PASS < /dev/tty
fi

#######################################
# 1. MongoDB bindIp
#######################################
log_info "[1/4] MongoDB bindIp guncelleniyor..."

# Build new bind IP list
ALL_IPS=$(hostname -I 2>/dev/null | tr ' ' '\n' | \
    grep -E '^[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+$' | \
    grep -v '^127\.' | awk '!seen[$0]++' | paste -sd, -)

if [ -n "$ALL_IPS" ]; then
    MONGO_BIND_IPS="127.0.0.1,${ALL_IPS}"
else
    MONGO_BIND_IPS="127.0.0.1,${NEW_IP}"
fi

sed -i "s/^  bindIp: .*/  bindIp: ${MONGO_BIND_IPS}/" /etc/mongod.conf
log_info "  bindIp: ${MONGO_BIND_IPS}"

systemctl restart mongod

# Wait for MongoDB
log_info "  MongoDB yeniden baslatiliyor..."
for i in $(seq 1 30); do
    if mongosh --quiet --eval "db.runCommand({ping:1}).ok" 2>/dev/null | grep -q "1"; then
        log_info "  MongoDB hazir (${i}s)"
        break
    fi
    if [ "$i" -eq 30 ]; then
        log_error "MongoDB baslatilamadi!"
        exit 1
    fi
    sleep 1
done

#######################################
# 2. MongoDB Replica Set reconfig
#######################################
log_info "[2/4] MongoDB Replica Set guncelleniyor..."

if ! mongosh "mongodb://admin:${MONGO_ADMIN_PASS}@localhost:27017/admin?authSource=admin&directConnection=true" --quiet --eval "
var cfg = rs.conf();
var oldHost = cfg.members[0].host;
cfg.members[0].host = '${NEW_IP}:27017';
cfg.version = cfg.version + 1;
rs.reconfig(cfg, {force: true});
print('Replica Set: ' + oldHost + ' -> ${NEW_IP}:27017');
"; then
    log_error "Replica Set guncellenemedi! MongoDB admin sifresi yanlis olabilir."
    log_error "Credential dosyasi: ${CRED_FILE}"
    log_error "Manuel deneyin: mongosh 'mongodb://admin:SIFRE@localhost:27017/admin?authSource=admin&directConnection=true'"
    exit 1
fi

# Wait for primary
log_info "  PRIMARY bekleniyor..."
for i in $(seq 1 60); do
    if mongosh "mongodb://admin:${MONGO_ADMIN_PASS}@localhost:27017/admin?authSource=admin&directConnection=true" \
        --quiet --eval 'db.hello().isWritablePrimary' 2>/dev/null | grep -q "true"; then
        log_info "  PRIMARY oldu (${i}s)"
        break
    fi
    if [ "$i" -eq 60 ]; then
        log_error "MongoDB PRIMARY olamadi!"
        exit 1
    fi
    sleep 1
done

#######################################
# 3. Let's Encrypt (if domain-based)
#######################################
log_info "[3/4] SSL sertifikasi kontrol ediliyor..."

DOMAIN=$(postconf -h myhostname 2>/dev/null)

if [ -d "/etc/letsencrypt/live/${DOMAIN}" ]; then
    log_info "  Let's Encrypt sertifikasi mevcut: ${DOMAIN}"
    log_warn "  DNS A kaydini yeni IP'ye (${NEW_IP}) guncellemeyi unutmayin!"
    log_info "  DNS yayildiktan sonra: certbot renew --force-renewal && systemctl restart postfix"
else
    log_info "  Let's Encrypt kullanilmiyor, atlaniyor"
fi

#######################################
# 4. Credentials dosyasi
#######################################
log_info "[4/4] Credentials dosyasi guncelleniyor..."

if [ -f "$CRED_FILE" ]; then
    # Update Server IP
    sed -i "s/Server IP:.*/Server IP:    ${NEW_IP}/" "$CRED_FILE"

    # Update connection strings
    if [ -n "$OLD_IP" ]; then
        sed -i "s/${OLD_IP}/${NEW_IP}/g" "$CRED_FILE"
    fi

    log_info "  ${CRED_FILE} guncellendi"
else
    log_warn "  Credentials dosyasi bulunamadi: ${CRED_FILE}"
fi

#######################################
# Summary
#######################################
echo ""
echo "=========================================="
echo -e "${GREEN}IP Guncelleme Tamamlandi!${NC}"
echo "=========================================="
echo ""
echo "  Yeni IP:    ${NEW_IP}"
if [ -n "$OLD_IP" ]; then
    echo "  Eski IP:    ${OLD_IP}"
fi
echo ""
echo "  MongoDB bindIp:      ${MONGO_BIND_IPS}"
echo "  Replica Set member:  ${NEW_IP}:27017"
echo ""
if [ -n "$DOMAIN" ]; then
    echo -e "  ${YELLOW}DNS kaydini guncellemeyi unutmayin:${NC}"
    echo "    ${DOMAIN} -> ${NEW_IP}"
    echo ""
fi
echo "  Dokunulmayan (IP bagimsiz):"
echo "    - relay-agent config (localhost kullanir)"
echo "    - Postfix ayarlari (domain bazli)"
echo "    - SMTP Filter (127.0.0.1:10025)"
echo ""
