"""
Real-time Delivery Log Watcher using MongoDB Change Streams

Bu servis, relay-agent'ın MongoDB'sindeki delivery loglarını
gerçek zamanlı olarak izler ve Mailgateway veritabanını günceller.

Kullanım:
    python delivery_watcher.py
    python delivery_watcher.py --verbose
"""

import asyncio
import logging
import os
import signal
import sys
from datetime import datetime
from typing import Optional

from motor.motor_asyncio import AsyncIOMotorClient
from pymongo.errors import PyMongoError
import asyncpg

# Logging setup
logging.basicConfig(
    level=logging.INFO,
    format='%(asctime)s - %(name)s - %(levelname)s - %(message)s'
)
logger = logging.getLogger('delivery_watcher')


class DeliveryWatcher:
    """MongoDB Change Stream watcher for delivery logs"""

    def __init__(self):
        # MongoDB settings (Relay Server)
        self.mongo_host = os.getenv('RELAY_MONGODB_HOST', 'localhost')
        self.mongo_port = int(os.getenv('RELAY_MONGODB_PORT', '27017'))
        self.mongo_db = os.getenv('RELAY_MONGODB_DATABASE', 'relay_logs')
        self.mongo_user = os.getenv('RELAY_MONGODB_USERNAME', 'mailgateway_reader')
        self.mongo_pass = os.getenv('RELAY_MONGODB_PASSWORD', '')

        # PostgreSQL settings (Mailgateway)
        self.pg_host = os.getenv('POSTGRES_HOST', 'localhost')
        self.pg_port = int(os.getenv('POSTGRES_PORT', '5432'))
        self.pg_db = os.getenv('POSTGRES_DB', 'mailgateway')
        self.pg_user = os.getenv('POSTGRES_USER', 'mailgateway')
        self.pg_pass = os.getenv('POSTGRES_PASSWORD', '')

        # Clients
        self.mongo_client: Optional[AsyncIOMotorClient] = None
        self.pg_pool: Optional[asyncpg.Pool] = None

        # Stats
        self.processed_count = 0
        self.error_count = 0
        self.running = True

    async def connect(self):
        """Connect to MongoDB and PostgreSQL"""
        # MongoDB connection
        mongo_uri = (
            f"mongodb://{self.mongo_user}:{self.mongo_pass}@"
            f"{self.mongo_host}:{self.mongo_port}/{self.mongo_db}"
            f"?authSource={self.mongo_db}&replicaSet=rs0"
        )

        logger.info(f"Connecting to MongoDB at {self.mongo_host}:{self.mongo_port}")
        self.mongo_client = AsyncIOMotorClient(mongo_uri)

        # Test connection
        await self.mongo_client.admin.command('ping')
        logger.info("MongoDB connection successful")

        # PostgreSQL connection pool
        logger.info(f"Connecting to PostgreSQL at {self.pg_host}:{self.pg_port}")
        self.pg_pool = await asyncpg.create_pool(
            host=self.pg_host,
            port=self.pg_port,
            database=self.pg_db,
            user=self.pg_user,
            password=self.pg_pass,
            min_size=2,
            max_size=10,
        )
        logger.info("PostgreSQL connection successful")

    async def close(self):
        """Close connections"""
        if self.mongo_client:
            self.mongo_client.close()
        if self.pg_pool:
            await self.pg_pool.close()
        logger.info(f"Connections closed. Processed: {self.processed_count}, Errors: {self.error_count}")

    async def watch(self):
        """Watch MongoDB change stream"""
        collection = self.mongo_client[self.mongo_db]['emails']

        # Change stream pipeline - only watch sent/bounced status
        pipeline = [
            {
                '$match': {
                    'operationType': {'$in': ['insert', 'update', 'replace']},
                    '$or': [
                        {'fullDocument.status': 'sent'},
                        {'fullDocument.status': 'bounced'},
                    ]
                }
            }
        ]

        logger.info("Starting change stream watcher...")

        while self.running:
            try:
                async with collection.watch(
                    pipeline,
                    full_document='updateLookup',
                    max_await_time_ms=1000
                ) as stream:
                    async for change in stream:
                        if not self.running:
                            break
                        await self.process_change(change)

            except PyMongoError as e:
                logger.error(f"MongoDB error: {e}")
                if self.running:
                    await asyncio.sleep(5)  # Wait before retry

    async def process_change(self, change: dict):
        """Process a single change event"""
        document = change.get('fullDocument')
        if not document:
            return

        mailgateway_queue_id = document.get('mailgateway_queue_id')
        if not mailgateway_queue_id:
            return

        try:
            await self.update_delivery_status(document)
            self.processed_count += 1

            if self.processed_count % 100 == 0:
                logger.info(f"Processed: {self.processed_count}, Errors: {self.error_count}")

        except Exception as e:
            self.error_count += 1
            logger.error(f"Failed to process {mailgateway_queue_id}: {e}")

    async def update_delivery_status(self, document: dict):
        """Update delivery status in Mailgateway PostgreSQL"""
        mailgateway_queue_id = document['mailgateway_queue_id']

        # Extract fields
        status = document.get('status')
        dsn = document.get('dsn')
        status_message = document.get('status_message')
        relay_queue_id = document.get('queue_id')
        relay_host = document.get('relay_host')
        relay_ip = document.get('relay_ip')
        delivery_time_ms = document.get('delivery_time_ms')
        delivered_at = document.get('delivered_at')

        # Convert datetime if needed
        if delivered_at and not isinstance(delivered_at, datetime):
            delivered_at = delivered_at.as_datetime() if hasattr(delivered_at, 'as_datetime') else None

        async with self.pg_pool.acquire() as conn:
            await conn.execute('''
                UPDATE emails SET
                    relay_status = $1,
                    relay_dsn = $2,
                    relay_message = $3,
                    relay_queue_id = $4,
                    relay_host = $5,
                    relay_ip = $6,
                    delivery_time_ms = $7,
                    delivered_at = $8,
                    updated_at = NOW()
                WHERE queue_id = $9
            ''',
                status,
                dsn,
                status_message,
                relay_queue_id,
                relay_host,
                relay_ip,
                delivery_time_ms,
                delivered_at,
                mailgateway_queue_id
            )

        logger.debug(f"Updated {mailgateway_queue_id} -> {status}")

    def stop(self):
        """Signal to stop watching"""
        self.running = False
        logger.info("Stop signal received")


async def main():
    watcher = DeliveryWatcher()

    # Signal handlers
    loop = asyncio.get_event_loop()
    for sig in (signal.SIGTERM, signal.SIGINT):
        loop.add_signal_handler(sig, watcher.stop)

    try:
        await watcher.connect()
        await watcher.watch()
    except KeyboardInterrupt:
        logger.info("Interrupted")
    finally:
        await watcher.close()


if __name__ == '__main__':
    # Verbose mode
    if '--verbose' in sys.argv or '-v' in sys.argv:
        logging.getLogger('delivery_watcher').setLevel(logging.DEBUG)

    asyncio.run(main())
