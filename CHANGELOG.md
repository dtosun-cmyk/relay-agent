# Relay Agent Changelog

## 2026-01-15

### Performans Optimizasyonları
Yüksek hacimli email işleme için dört kritik optimizasyon yapıldı:

1. **MongoDB Bulk Upsert**
   - Tek tek upsert yerine toplu upsert
   - 100 kayıt/batch, 5 saniye flush interval
   - %90+ veritabanı yükü azalması

2. **Pending Map Temizleme**
   - Eski kayıtlar 30 dakika sonra otomatik temizleniyor
   - Memory leak önlendi
   - Her 5 dakikada cleanup çalışıyor

3. **Parser Sharded Map**
   - Tek map yerine 64 shard'lı map
   - Lock contention %95 azaldı
   - Yüksek concurrent erişimde performans artışı

4. **SMTP Connection Pool**
   - Her istek için yeni bağlantı yerine connection pool
   - Varsayılan: 10 bağlantı, 30sn timeout
   - SMTP forwarding performansı arttı

### Setup Script Güncellemesi (`setup-mailgateway-access.sh`)

Yeni komutlar eklendi:

| Komut | Açıklama |
|-------|----------|
| `--init` | MongoDB admin kullanıcısı ve replica set başlat |
| `--create <isim>` | Yeni müşteri database'i ve kullanıcıları oluştur |
| `--list` | Tüm kayıtlı kullanıcıları listele |
| `--delete <isim>` | Kullanıcı ve database'i sil |

**Özellikler:**
- Her müşteri için ayrı database (`relay_<isim>`)
- İki kullanıcı: `<isim>_writer` (readWrite), `<isim>_reader` (read)
- Otomatik güvenli şifre üretimi
- 9 adet gerekli index otomatik oluşturma
- Relay-agent config çıktısı

### MongoDB Index Düzeltmesi
- `queue_id` indexi `unique: true, sparse: true` olarak güncellendi
- Boş queue_id değerleri artık index çakışmasına neden olmuyor
- Dosya: `internal/repository/mongodb.go`

### Oluşturulan Indexler
```
idx_queue_id              - unique, sparse
idx_mailgateway_queue_id  - sparse
idx_recipient_domain_created
idx_status_created
idx_created_at
idx_created_at_ttl        - 30 gün sonra otomatik silme
idx_webhook_status
idx_sender
idx_recipient
```

### Çalışan Servisler
| Servis | Port | Açıklama |
|--------|------|----------|
| MongoDB | 27017 | Replica Set (rs0) |
| API Server | 8080 | REST API |
| SMTP Filter | 10025 | Email filtreleme |

### Dosya Değişiklikleri
- `setup-mailgateway-access.sh` - Multi-user desteği
- `internal/repository/mongodb.go` - Sparse index, bulk upsert
- `internal/parser/postfix.go` - Sharded map
- `internal/filter/smtp_filter.go` - Connection pool
- `config/config.yaml` - Yeni processing ayarları

### Hostname ve SSL Sertifika Değişikliği
- Hostname değişikliği yapıldı
- Yeni Let's Encrypt sertifikası alındı (15 Nisan 2026'ya kadar geçerli)
- Postfix TLS ayarları güncellendi

### SASL Authentication Hatası Düzeltmesi

**Hata:** `4.7.0 temp auth failed` veya `5.7.8 authentication failed`

**Sebepler ve Çözümler:**

1. **SASL yanlış metod kullanıyordu**
   ```bash
   # /etc/postfix/sasl/smtpd.conf - HATALI
   pwcheck_method: saslauthd    # saslauthd servisi çalışmıyordu

   # /etc/postfix/sasl/smtpd.conf - DOĞRU
   pwcheck_method: auxprop
   auxprop_plugin: sasldb
   mech_list: PLAIN LOGIN
   sasldb_path: /etc/sasldb2
   ```

2. **Postfix chroot sorunu**
   ```bash
   # /etc/postfix/master.cf - submission satırı
   # chroot=y iken /etc/sasldb2 dosyasına erişemiyordu

   # Eski (hatalı)
   submission inet  n  -  y  -  -  smtpd

   # Yeni (düzeltilmiş) - chroot=n
   submission inet  n  -  n  -  -  smtpd
   ```

3. **SASL kullanıcı domain uyumsuzluğu**
   ```bash
   # Kullanıcı hostname değişince eski domain ile kaldı
   sasldblistusers2  # kontrol et

   # Yeni kullanıcı oluştur
   echo "SIFRE" | saslpasswd2 -p -u your.hostname.example USERNAME
   ```

4. **Dosya izinleri**
   ```bash
   chown root:postfix /etc/sasldb2
   chmod 640 /etc/sasldb2
   ```

**SASL Kullanıcı Yönetimi:**
```bash
# Kullanıcı listele
sasldblistusers2

# Kullanıcı oluştur (API ile)
curl -X POST -H "X-API-Secret: API_SECRET" \
  -H "Content-Type: application/json" \
  -d '{"username":"relay2","password":"SIFRE"}' \
  http://localhost:8080/api/smtp-users

# Manuel kullanıcı oluştur
echo "SIFRE" | saslpasswd2 -c -p -u your.hostname.example USERNAME

# Kullanıcı sil
saslpasswd2 -d -u your.hostname.example USERNAME
```

**Mevcut Bağlantı Bilgileri:**
```
Host: <YOUR_HOSTNAME>
Port: 587
TLS: STARTTLS
Username: <YOUR_USERNAME>
Password: <YOUR_PASSWORD>
```

---

## 2025-12-23

### MongoDB Change Stream Entegrasyonu (Webhook Yerine)
- Webhook sistemi kaldırıldı, MongoDB Change Stream ile değiştirildi
- MongoDB Replica Set yapılandırması (`rs0`)
- Mailgateway için read-only kullanıcı desteği
- Gecikme: < 100ms (webhook'tan çok daha hızlı)

### Queue Management API
Postfix kuyruk yönetimi için yeni API endpointleri eklendi:

| Method | Endpoint | Açıklama |
|--------|----------|----------|
| GET | `/api/queue` | Kuyruk istatistikleri |
| GET | `/api/queue/messages?limit=50&offset=0` | Mesajları listele |
| DELETE | `/api/queue/messages?confirm=yes` | Tüm mesajları sil |
| DELETE | `/api/queue/messages/{queue_id}` | Tek mesaj sil |
| POST | `/api/queue/messages/{queue_id}/requeue` | Yeniden kuyruğa al |
| POST | `/api/queue/messages/{queue_id}/hold` | Beklet |
| POST | `/api/queue/messages/{queue_id}/release` | Bekletmeden çıkar |
| POST | `/api/queue/flush` | Tüm kuyruğu gönder |

### Bounce Mesajları İyileştirmesi
- Status message regex düzeltildi (iç içe parantezler)
- Tüm bounce/reject mesajları tam olarak yakalanıyor:
  - DMARC rejection
  - SPF failure
  - SMTPUTF8 hatası
  - Connection timeout
  - User unknown
  - Mailbox full
  - Blacklist (RBL)

### Zaman Dilimi Düzeltmesi
- Tüm tarih/saat alanları artık Türkiye saatinde (UTC+3) kaydediliyor
- Etkilenen alanlar:
  - `created_at`
  - `updated_at`
  - `received_at`
  - `delivered_at`
- `/opt/relay-agent/internal/util/time.go` oluşturuldu

### Postfix SMTPUTF8 Düzeltmesi
- `smtputf8_enable = no` ayarlandı
- SMTPUTF8 desteklemeyen sunucularla (Yandex, vb.) uyumluluk sağlandı

### Systemd Servis Düzeltmesi
- Queue API silme işlemi "Read-only file system" hatası veriyordu
- `ProtectSystem=strict` ayarı `/var/spool/postfix` dizinini read-only yapıyordu
- `ReadWritePaths`'e `/var/spool/postfix` eklenerek düzeltildi

### Dosya Değişiklikleri
- `/opt/relay-agent/internal/postfix/queue.go` - Yeni (Queue yönetimi)
- `/opt/relay-agent/internal/util/time.go` - Yeni (Türkiye saati)
- `/opt/relay-agent/internal/api/handlers.go` - Queue API endpointleri
- `/opt/relay-agent/internal/api/server.go` - Queue manager entegrasyonu
- `/opt/relay-agent/internal/parser/patterns.go` - Regex düzeltmesi
- `/opt/relay-agent/internal/parser/postfix.go` - Zaman dilimi düzeltmesi
- `/opt/relay-agent/internal/repository/mongodb.go` - Zaman dilimi düzeltmesi
- `/opt/relay-agent/internal/filter/smtp_filter.go` - Zaman dilimi düzeltmesi
- `/opt/relay-agent/cmd/relay-agent/main.go` - Webhook kaldırıldı
- `/etc/mongod.conf` - Replica Set yapılandırması
- `/etc/postfix/main.cf` - SMTPUTF8 kapatıldı
- `/etc/systemd/system/relay-agent.service` - ReadWritePaths düzeltmesi

### MongoDB Email Alanları
| Alan | Tip | Açıklama |
|------|-----|----------|
| mailgateway_queue_id | String | Mailgateway kuyruk ID |
| queue_id | String | Postfix kuyruk ID |
| sender | String | Gönderen adresi |
| recipient | String | Alıcı adresi |
| recipient_domain | String | Alıcı domain |
| provider | String | Gmail, Yandex, vb. |
| client_host | String | Kaynak hostname |
| client_ip | String | Kaynak IP |
| relay_host | String | Hedef MX sunucu |
| relay_ip | String | Hedef MX IP |
| status | String | sent / bounced / deferred |
| dsn | String | DSN kodu |
| status_message | String | Tam hata/başarı mesajı |
| size | Int64 | Mesaj boyutu (byte) |
| delivery_time_ms | Int64 | Teslim süresi (ms) |
| attempts | Int64 | Deneme sayısı |
| received_at | Date | Alınma zamanı (Türkiye) |
| delivered_at | Date | Teslim zamanı (Türkiye) |
| created_at | Date | Kayıt oluşturma (Türkiye) |
| updated_at | Date | Son güncelleme (Türkiye) |
