# What is ZKTeco ADMS?

Inspired by Herbin Tsobeng

[Link-1](https://www.linkedin.com/pulse/zkteco-adms-protocol-link-your-zk-device-server-herbin-tsobeng-qg0ze/)
[Link-2](https://stackoverflow.com/questions/65844119/zkteco-push-sdk/72994156)

Enhanced with commands were discovered by probing a real ZKTeco device and
observing the actual HTTP traffic 

ADMS = Automatic Data Master Server (sometimes also referred to as Attendance Device Management System). It’s the proprietary communication protocol used by ZKTeco biometric and RFID terminals (fingerprint, face, card readers, etc.) to send real-time attendance and access logs to a central server.

The protocol is at the heart of ZKTeco’s cloud-based attendance management. Devices don’t just store logs locally — they push logs, user data, and status updates to an ADMS server over HTTP(S).

## How it Works

1. Device Initialization

Each device is configured with:

Server URL (e.g., http://server-ip/icc/ or custom endpoint)
Device serial number (SN)
Communication key (optional security)

On boot, the device registers itself with the ADMS server.

2. Communication Channel

Transport: HTTP (or HTTPS on newer firmwares).
Method: Standard HTTP POST/GET requests.
Format: Query parameters + plain-text body (not JSON/XML, mostly custom key=value pairs).

3. Request Types

Device → Server

c=registry → Device registration
c=log → Push attendance/access logs
c=options → Send device options/config
c=photo → Upload photos (if supported)

Server → Device

Returns plain text commands (OK, FAIL, USER ADD, USER DEL, etc.)
Commands are queued until device fetches them.

4. Data Format

Logs are often posted as text lines:

```
PIN=12345    DateTime=2025-09-02 14:32:11    Verified=1    Status=0
User upload/download is similar, e.g.:

PIN=12345    Name=John Doe    Privilege=0    Card=12345678
Features of the ADMS Protocol
```

Push Mechanism: Logs are sent to the server immediately (not just polled).
Two-way Sync: Server can remotely manage users, fingerprints, and settings.
Lightweight: Uses very simple HTTP/text instead of complex APIs.
Stateless: Each request is standalone, relies on SN + key for identification.
Compatible: Works across many ZKTeco device families (iClock, ZKTime, SilkBio, etc.).

## Limitations

Proprietary: Not officially published in full detail (info comes from reverse-engineering and community docs).
Security: Plain HTTP by default; encryption is weak unless forced over HTTPS.
Legacy Style: No REST/JSON standardization — integration can be messy.

ZKTeco ADMS exchange (simplified, but accurate)

1. Device Registration (first contact)

HTTP Request (from device → server)

```
POST /iclock/cdata?SN=AC123456789&table=options&c=registry HTTP/1.1
Host: yourserver.com
Content-Type: application/x-www-form-urlencoded
Content-Length: 0
```

Server Response

```
OK

```

This tells the device “you’re registered, continue sending data.”

2. Sending Attendance Logs

```
HTTP Request

POST /iclock/cdata?SN=AC123456789&table=ATTLOG&c=log HTTP/1.1
Host: yourserver.com
Content-Type: text/plain
Content-Length: 128

PIN=1001    DateTime=2025-09-02 14:32:11    Verified=1    Status=0
PIN=1002    DateTime=2025-09-02 14:35:54    Verified=1    Status=0
PIN = user ID
DateTime = timestamp of check-in/out
Verified = verification mode (0=password, 1=fingerprint, 4=card, 15=face, 25=palm, etc.)
Status = check type (0=Check In, 1=Check Out, 2=Break Out, 3=Break In, 4=Overtime In, 5=Overtime Out)
```

Server Response

```
OK
```
3. Sending User Data

```
HTTP Request

POST /iclock/cdata?SN=AC123456789&table=USER&c=data HTTP/1.1
Host: yourserver.com
Content-Type: text/plain

PIN=1001    Name=John Doe    Privilege=0    Card=12345678
PIN=1002    Name=Alice Smith Privilege=14   Card=87654321
Privilege=0 → Normal user
Privilege=14 → Admin
```

Server Response

```
OK
```

4. Server Commands → Device

Sometimes the device polls for commands.

Device Poll

```
GET /iclock/getrequest?SN=AC123456789 HTTP/1.1
Host: yourserver.com
Server Response

USER ADD PIN=1003    Name=Bob Marley    Privilege=0    Card=99887766
USER DEL PIN=1002
```

The device will execute these and then respond "OK".

> **⚠ Correction (confirmed on real hardware):** The `USER ADD` / `USER DEL`
> syntax shown above is from the original datasheet but is **rejected by real
> devices** (ZAM180-NF firmware, Return=-1002). See
> [Confirmed Server→Device Commands](#confirmed-serverdevice-commands) below
> for the correct wire formats.

5. Photo / Face Templates (optional)

Some devices also send photos or templates with:

```
POST /iclock/cdata?SN=AC123456789&table=PHOTO&c=upload
```
(binary data in body)

As you can see, it’s just HTTP + plain text with key=value pairs, very different from REST/JSON APIs. The whole flow is lightweight and easy to parse, which is why ZKTeco can support it even on low-power terminals.

Code exemple with Python/Flask

```
from flask import Flask, request

app = Flask(__name__)

@app.route("/iclock/cdata", methods=["GET", "POST"])
def cdata():
    sn = request.args.get("SN")
    table = request.args.get("table")
    command = request.args.get("c")

    print(f"📡 Device SN={sn}, table={table}, command={command}")

    if table == "ATTLOG" and request.data:
        logs = request.data.decode("utf-8").strip().splitlines()
        for line in logs:
            print("   → Log:", line)
        return "OK"

    if table == "USER" and request.data:
        users = request.data.decode("utf-8").strip().splitlines()
        for line in users:
            print("   → User:", line)
        return "OK"

    return "OK"


@app.route("/iclock/getrequest", methods=["GET"])
def getrequest():
    sn = request.args.get("SN")
    print(f"📡 Device {sn} is polling for commands")

    # Example: tell device to add a user
    return "USER ADD PIN=2001\tName=Test User\tPrivilege=0\tCard=12345678"


if __name__ == "__main__":
    app.run(host="0.0.0.0", port=8000, debug=True)
```

What this server does

Accepts logs (ATTLOG)
Accepts user sync (USER)
Returns OK to acknowledge
Responds to getrequest with commands (like adding/removing users)

This is enough to get a ZKTeco device talking to your server. In production you’d typically:

Save logs/users in a database
Implement authentication (commKey)
Use HTTPS
Add an admin dashboard

---

# Real Device Protocol Findings

The information below was discovered by probing a real ZKTeco device and
observing the actual HTTP traffic. The test device:

- **Model**: SpeedFace-V5L-RFID[TI]
- **Firmware**: ZAM180-NF-Ver1.1.17
- **User-Agent**: `iClock Proxy/1.09`
- **Platform**: ZAM180_TFT
- **MAC**: 00:17:61:12:f8:db

## Command Wire Format

When the device polls `GET /iclock/getrequest?SN=...`, pending commands are
delivered in the response body using this format:

```
C:<ID>:<CMD>\n
```

Where `<ID>` is a monotonically increasing integer assigned by the server.
Multiple commands can be sent in a single response:

```
C:1:INFO
C:2:CHECK
C:3:DATA UPDATE USERINFO PIN=1001	Name=John Doe	Privilege=0	Card=12345678
```

## Command Confirmation

After executing each command, the device POSTs the result to
`/iclock/devicecmd?SN=...`:

```
ID=1&Return=0&CMD=INFO
```

### Batched Confirmations

The device **batches multiple confirmations** in a single POST body,
one per line:

```
ID=1&Return=0&CMD=DATA
ID=2&Return=0&CMD=DATA
ID=3&Return=0&CMD=CHECK
```

### Shell Command Response Format

Shell command confirmations use a multiline format with a `Content` field:

```
ID=32
Return=0
CMD=Shell
Content=Tue Mar 24 16:12:26 GMT 2026
```

## Command Return Codes

| Code | Meaning |
|------|---------|
| `0` | Success |
| `-1` | Command not supported, or no data available |
| `-2` | File operation failed |
| `-1002` | Invalid command syntax |
| `-1004` | Table/feature not supported on this device model |

## Confirmed Server→Device Commands

### ACCEPTED (Return=0)

| Command | CMD echo | Description |
|---------|----------|-------------|
| `INFO` | `INFO` | Returns full device info (firmware, serial, capabilities) in the response body of the next `/iclock/cdata` POST |
| `CHECK` | `CHECK` | Heartbeat / connectivity check |
| `LOG` | `LOG` | Request log data |
| `Shell <command>` | `Shell` | Execute an arbitrary shell command on the device's Linux OS. Output returned in `Content=` field. **Use with extreme caution.** |

#### GET OPTION

```
GET OPTION FROM <key>
```

CMD echo: `GET OPTION`. All of the following keys were accepted:

| Key | Example Value | Description |
|-----|---------------|-------------|
| `DeviceName` | `SpeedFace-V5L-RFID[TI]` | Device model name |
| `FWVersion` | `ZAM180-NF-Ver1.1.17` | Firmware version |
| `IPAddress` | `192.168.1.235` | Device IP address |
| `MACAddress` | `00:17:61:12:f8:db` | Device MAC address |
| `Platform` | `ZAM180_TFT` | Hardware platform |
| `WorkCode` | `0` | Work code setting |
| `LockCount` | `0` | Door lock count |
| `UserCount` | `2` | Current enrolled users |
| `FPCount` | `0` | Fingerprint templates |
| `AttLogCount` | `0` | Attendance log entries |
| `FaceCount` | `2` | Face templates |
| `TransactionCount` | `0` | Transaction count |
| `MaxUserCount` | `6000` | Max user capacity |
| `MaxAttLogCount` | `200000` | Max attendance log capacity |
| `MaxFingerCount` | `6000` | Max fingerprint capacity |
| `MaxFaceCount` | `1200` | Max face template capacity |

#### User Management

**Add / update user** (tab-separated key=value pairs):

```
DATA UPDATE USERINFO PIN=<pin>\tName=<name>\tPrivilege=<priv>\tCard=<card>
```

CMD echo: `DATA`. Privilege values: `0` = normal user, `14` = admin.

**Delete user:**

```
DATA DELETE USERINFO PIN=<pin>
```

CMD echo: `DATA`.

> **Important:** The original datasheet shows `USER ADD` and `USER DEL` —
> these are **rejected** by real devices with Return=-1002. The `DATA DEL`
> shorthand (without the full word `DELETE`) is also rejected. Always use
> `DATA UPDATE USERINFO` and `DATA DELETE USERINFO`.

#### Data Queries

```
DATA QUERY USERINFO            # Query all users
DATA QUERY USERINFO PIN=<n>    # Query a single user by PIN
```

CMD echo: `DATA`. Query results are **not** returned in the command
confirmation. Instead, the device pushes data via `POST /iclock/cdata`
with `table=OPERLOG` or other table parameters.

### REJECTED

| Command | Return | Meaning |
|---------|--------|---------|
| `DATA QUERY ATTLOG` | `-1` | No data available (empty attendance log) |
| `PutFile FileName=options.cfg` | `-1` | File transfer failed (device attempted `GET /iclock/FileName=options.cfg?` but server wasn't serving the file) |
| `GetFile FileName=options.cfg` | `-2` | File operation failed |
| `DATA QUERY OPTIONS` | `-1004` | Table not supported |
| `DATA QUERY TIMEZONE` | `-1004` | Table not supported (no access control on this model) |
| `DATA QUERY FIRSTCARD` | `-1004` | Table not supported |
| `DATA QUERY MULTIMCARD` | `-1004` | Table not supported |
| `DATA QUERY INOUTFUN` | `-1004` | Table not supported |
| `USER ADD PIN=...` | `-1002` | Invalid syntax (use `DATA UPDATE USERINFO` instead) |
| `USER DEL PIN=...` | `-1002` | Invalid syntax (use `DATA DELETE USERINFO` instead) |
| `DATA DEL USERINFO PIN=...` | `-1002` | Invalid syntax (must be `DATA DELETE`, not `DATA DEL`) |

## Data Push Behavior

When the device processes `DATA QUERY` commands, it does **not** return
data in the command confirmation body. Instead it pushes data via separate
HTTP requests:

| Push endpoint | Content |
|---------------|---------|
| `POST /iclock/cdata?table=OPERLOG` | User records, operation logs |
| `POST /iclock/cdata?table=BIODATA` | Face/fingerprint templates (binary) |
| `POST /iclock/cdata?table=BIOPHOTO` | User photos (JPEG binary) |
| `POST /iclock/cdata?table=options` | Device configuration options |

## Device Polling Behavior

- The device polls `GET /iclock/getrequest?SN=...` every **3–8 seconds**.
- If no commands are pending, the server responds with `OK`.
- The device identifies itself with `User-Agent: iClock Proxy/1.09`
  (version varies by firmware).

## PutFile Behavior

When a `PutFile FileName=<name>` command is queued, the device attempts to
download the file by issuing:

```
GET /iclock/FileName=<name>? HTTP/1.1
```

The server must serve the file content at that path for the operation to
succeed. If the file is not available, the device reports Return=-1.
