"""Hard boundary for real evaluation work; intentionally separate from reset administration."""

TARGET_REPOSITORY = "zymooll/ai-workflow-alpha-test"
TARGET_REPOSITORY_ID = 1416790756
TARGET_APP_ID = 5285791
TARGET_OWNER_LOGIN = "zymooll"
TARGET_OWNER_ID = 51841832


def validate_identity(repository, repository_id, installation_id, app_id):
    """Validate caller supplied target pins before any GitHub request or write."""
    if repository != TARGET_REPOSITORY or type(repository_id) is not int or repository_id != TARGET_REPOSITORY_ID:
        raise ValueError("评估仅允许用户指定的测试仓库及其固定数字 ID")
    if type(installation_id) is not int or installation_id <= 0 or type(app_id) is not int or app_id != TARGET_APP_ID:
        raise ValueError("评估必须显式提供有效安装 ID，并绑定已指定的 GitHub App ID")
