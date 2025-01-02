terraform {
  required_providers {
    yandex = {
      source = "yandex-cloud/yandex"
    }
  }
  required_version = ">= 0.13"
}

provider "yandex" {
  zone = "ru-central1-a"
  service_account_key_file = pathexpand("~/.yc-keys/key.json")

  cloud_id  = var.cloud_id
  folder_id = var.folder_id
}

resource "yandex_iam_service_account" "sa" {
 name = "hw1-sa"
 folder_id = var.folder_id
}

resource "yandex_resourcemanager_folder_iam_member" "sa-admin" {
  folder_id = var.folder_id
  role      = "admin"
  member    = "serviceAccount:${yandex_iam_service_account.sa.id}"
}

resource "yandex_iam_service_account_static_access_key" "sa-static-key" {
  service_account_id = yandex_iam_service_account.sa.id
}

resource "yandex_iam_service_account_api_key" "sa-api-key" {
  service_account_id = yandex_iam_service_account.sa.id
}

resource "yandex_storage_bucket" "hw1" {
  access_key            = yandex_iam_service_account_static_access_key.sa-static-key.access_key
  secret_key            = yandex_iam_service_account_static_access_key.sa-static-key.secret_key
  bucket                = "gpt-instructions"
  default_storage_class = "STANDARD"
}

resource "yandex_storage_object" "gpt_instruction" {
  access_key = yandex_iam_service_account_static_access_key.sa-static-key.access_key
  secret_key = yandex_iam_service_account_static_access_key.sa-static-key.secret_key
  bucket = yandex_storage_bucket.hw1.bucket
  key    = "instruction.txt"
  source = "./static/instruction.txt"
}

resource "yandex_function" "telegram-bot-func" {
  name        = "telegram-bot-func"

  folder_id = var.folder_id
  user_hash = "0.0.28"
  runtime   = "golang121"
  entrypoint = "index.Handle"
  memory             = "128"
  execution_timeout  = "30"
  service_account_id = yandex_iam_service_account.sa.id

  content {
    zip_filename = "../index0.0.28.zip"
  }

  environment = {
    TG_BOT_KEY          = var.tg_bot_key
    VISION_API_KEY      = yandex_iam_service_account_api_key.sa-api-key.secret_key
    YAGPT_API_KEY = yandex_iam_service_account_api_key.sa-api-key.secret_key
    FOLDER_ID = var.folder_id
    S3_ACCESS_KEY = yandex_iam_service_account_static_access_key.sa-static-key.access_key
    S3_SECRET_KEY = yandex_iam_service_account_static_access_key.sa-static-key.secret_key
    YAGPT_INSTRUCTION_PATH = "https://storage.yandexcloud.net/${yandex_storage_bucket.hw1.bucket}/${yandex_storage_object.gpt_instruction.key}"
  }
}

resource "yandex_function_iam_binding" "function-iam" {
  function_id = yandex_function.telegram-bot-func.id
  role        = "functions.functionInvoker"
  members = [
    "system:allUsers",
  ]
}

resource "yandex_api_gateway" "tg-api-gateway" {
  name        = "tg-api-gateway"
  spec = <<-EOT
    openapi: "3.0.0"
    info:
      version: 0.0.1
      title: hw1
    paths:
      /telegram-bot-func:
        post:
          x-yc-apigateway-integration:
            type: cloud_functions
            function_id: "${yandex_function.telegram-bot-func.id}"
            service_account_id: "${yandex_iam_service_account.sa.id}"
          operationId: telegram-bot-func
  EOT
}

resource "null_resource" "telegram_webhook" {
  provisioner "local-exec" {
    command = "curl -X POST https://api.telegram.org/bot${var.tg_bot_key}/setWebhook?url=${yandex_api_gateway.tg-api-gateway.domain}/telegram-bot-func"
  }

  triggers = {
    webhook_url = yandex_api_gateway.tg-api-gateway.domain
  }
}

resource "null_resource" "telegram_webhook_remove" {
  triggers = {
    destroy_trigger = "remove_webhook"
  }

  provisioner "local-exec" {
    command = "curl https://api.telegram.org/bot${var.tg_bot_key}/deleteWebhook?drop_pending_updates=True"
  }

  lifecycle {
    prevent_destroy = false
  }
}