# Mailgateway Integration - Real-Time Delivery Logs

Bu klasör, Mailgateway'in relay-agent'tan gerçek zamanlı teslim loglarını alması için gerekli dosyaları içerir.

## Mimari

```
Relay Server                            Mailgateway Server
┌─────────────────────────┐          ┌─────────────────────────┐
│  Relay Agent            │          │  Laravel                │
│  ├─ SMTP Filter         │          │  ├─ WatchDeliveryLogs   │
│  ├─ Log Parser          │          │  │   (Change Stream)    │
│  └─ MongoDB ────────────┼──────────┼──► Emails Table         │
│      (emails collection)│  TCP/27017 │                       │
└─────────────────────────┘          └─────────────────────────┘
```

## Kurulum

### 1. MongoDB PHP Extension (Mailgateway)

```bash
pecl install mongodb
echo "extension=mongodb.so" >> /etc/php/8.2/cli/php.ini
```

### 2. Laravel MongoDB Package

```bash
composer require mongodb/laravel-mongodb
```

### 3. Dosyaları Kopyala

```bash
# Command dosyasını kopyala
cp WatchDeliveryLogs.php /var/www/mailgateway/app/Console/Commands/

# Database config'i ekle (config/database.php connections array'ine)
# database_config.php dosyasındaki içeriği ekle

# .env dosyasına ekle
cat >> /var/www/mailgateway/.env << 'EOF'
RELAY_MONGODB_HOST=YOUR_RELAY_SERVER_IP
RELAY_MONGODB_PORT=27017
RELAY_MONGODB_DATABASE=relay_logs
RELAY_MONGODB_USERNAME=mailgateway_reader
RELAY_MONGODB_PASSWORD=YOUR_PASSWORD
EOF
```

### 4. Supervisor Kurulumu

```bash
cp supervisor_delivery_watcher.conf /etc/supervisor/conf.d/delivery-watcher.conf
supervisorctl reread
supervisorctl update
supervisorctl start delivery-watcher
```

## Test

```bash
# Manuel test
php artisan delivery:watch --verbose

# Supervisor durumu
supervisorctl status delivery-watcher
```

## Bağlantı Bilgileri

| Parametre | Değer |
|-----------|-------|
| Host | YOUR_RELAY_SERVER_IP |
| Port | 27017 |
| Database | relay_logs |
| Collection | emails |
| Username | mailgateway_reader |
| Password | (setup-mailgateway-access.sh ile oluşturulur) |
| Replica Set | rs0 |

## Güvenlik

- MongoDB sadece Mailgateway IP'sinden erişilebilir (firewall ile kısıtlayın)
- Read-only kullanıcı ile bağlantı
- TLS/SSL eklenebilir (production için önerilir)

## Gecikme

- MongoDB Change Stream ile gecikme: **< 100ms**
- Webhook'tan çok daha hızlı ve güvenilir
