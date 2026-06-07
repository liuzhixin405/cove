# Cove User Manual

## CovePhone (Android)

CovePhone is the Android companion app for cove.

### Requirements

- Android 8.0 (API 26) or higher
- Internet connection for API access
- A valid API key from a supported provider (e.g., DeepSeek)

### Installation

1. Download the latest APK from the [Releases page](https://github.com/liuzhixin405/cove/releases) or directly: [covephone-v4.0.5.apk](../dist/v4.0.5/covephone-v4.0.5.apk)
2. Enable "Install from unknown sources" on your Android device
3. Open the APK file and follow the installation prompts

### Setup

1. Launch CovePhone
2. Go to Settings (gear icon)
3. Enter your API key
4. Select your model and provider
5. Return to the chat screen and start asking questions!

### Features

- **Real AI Engine**: Uses the same Go-powered engine as the desktop CLI, compiled for Android via `gomobile`
- **Thinking Display**: AI thinking process shown with smooth scrolling
- **Persistent Settings**: API key, model, and provider settings are saved automatically
- **Multi-turn Conversation**: Full chat history within a session

### Troubleshooting

If the app returns the same response regardless of input:
1. Check that your API key is correctly configured
2. Ensure you have an active internet connection
3. Try switching to a different model
4. Restart the app

### Support

For technical support, contact: the project maintainer via GitHub Issues.